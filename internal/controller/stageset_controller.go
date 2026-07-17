// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

// Package controller implements the StageSet reconciler.
package controller

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	fluxmeta "github.com/fluxcd/pkg/apis/meta"
	fluxconditions "github.com/fluxcd/pkg/runtime/conditions"
	"github.com/fluxcd/pkg/runtime/jitter"
	fluxpatch "github.com/fluxcd/pkg/runtime/patch"
	fluxpredicates "github.com/fluxcd/pkg/runtime/predicates"
	"github.com/fluxcd/pkg/ssa"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	crbuilder "sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/actions"
	"github.com/metio/stageset-controller/internal/apply"
	"github.com/metio/stageset-controller/internal/artifact"
	"github.com/metio/stageset-controller/internal/build"
	"github.com/metio/stageset-controller/internal/decryptor"
	"github.com/metio/stageset-controller/internal/inventory"
	"github.com/metio/stageset-controller/internal/metrics"
	"github.com/metio/stageset-controller/internal/metricsource"
	"github.com/metio/stageset-controller/internal/observability"
	"github.com/metio/stageset-controller/internal/stageinv"
)

// defaultIntervalJitterFraction is the +/- fraction applied to every
// interval-based RequeueAfter so a fleet of StageSets configured with the same
// interval doesn't reconcile in lockstep. 5% spreads the wakeups without
// meaningfully shifting the effective cadence.
const defaultIntervalJitterFraction = 0.05

// defaultMaxTeardownWait is how long a deleting StageSet's finalizer holds
// while reverse-order teardown keeps failing before reconcileDelete force-drops
// it. One hour is generous enough to ride out a transient target-cluster
// outage but short enough that a permanently-unreachable target (deleted
// kubeConfig Secret, revoked RBAC, decommissioned remote cluster) doesn't pin
// the StageSet in Terminating forever and block namespace teardown.
const defaultMaxTeardownWait = 1 * time.Hour

// permanentRetryInterval bounds how soon a StageSet sitting on a terminal
// Ready=False reason re-enters the workqueue. Terminal failures (RBAC denied,
// invalid spec, a dependsOn cycle, an invalid version) don't engage
// controller-runtime's error backoff — the reconcile returns no error so the
// queue doesn't spin. But several of them heal out-of-band without producing a
// watch event the StageSet sees: granting the tenant SA an RBAC verb, fixing a
// referenced source in another namespace, or breaking a dependsOn cycle. A
// bounded RequeueAfter gives those a self-healing re-check roughly once a
// minute without hot-looping — the gap between "operator grants RBAC" and
// "StageSet recovers" stays at worst this interval.
const permanentRetryInterval = 1 * time.Minute

// StageSetReconciler reconciles StageSet objects. The reconciliation model —
// resolve and pin artifacts, then BUILD -> APPLY -> PRUNE -> VERIFY each stage
// in order, with a finalizer for teardown — is the contract documented for users
// under docs/content/ (the api/ and usage/ sections).
type StageSetReconciler struct {
	client.Client

	// APIReader is an uncached reader against the controller's own cluster, used
	// for the promotion restart/event gates' pod and event reads AND the
	// cross-object dependency/source graph walk. Going direct to the apiserver
	// avoids spinning up cluster-wide pod/event informers behind the cached
	// client, and — for the graph walk — avoids the manager's cache scoping: with
	// --watch-namespaces set, a cached Get for a dependency in an unwatched
	// namespace returns "unknown namespace for the cache" (not NotFound), which
	// the walk would misclassify as a transient error and back off on forever.
	// Reading uncached matches what the actual apply/fetch path sees. Set from
	// mgr.GetAPIReader() in SetupWithManager; tests may leave it nil to fall back
	// to the (fake) cached client.
	APIReader client.Reader

	// Config is the manager's rest config, cloned per tenant to build the
	// tenant-scoped clients spec.serviceAccountName requires. Set in
	// SetupWithManager; leaving it nil disables local-cluster identity
	// assumption (tests that never set serviceAccountName).
	Config *rest.Config

	// SkipImpersonation disables the local-cluster TokenRequest mint: when
	// true, a StageSet carrying spec.serviceAccountName applies under the
	// controller's own client rather than the tenant SA's identity. ONLY the
	// envtest harness sets this — production must keep it false so a tenant's
	// RBAC bounds what its StageSets touch. SkipImpersonation governs only the
	// local-cluster mint; the remote-cluster path (spec.kubeConfig) never mints.
	SkipImpersonation bool

	// minter mints short-lived TokenRequest tokens for the tenant SAs the
	// local-cluster apply assumes. Defaulted from a kubernetes.Clientset
	// built off Config in SetupWithManager; tests substitute a fake.
	minter tokenMinter
	// tokens caches minted tokens per (namespace, SA) with expiry-aware
	// refresh, so steady reconcile load doesn't hammer the TokenRequest API.
	tokens *tokenCache

	// RESTMapper resolves GVKs for the SSA status poller. Defaults to the
	// manager's mapper in SetupWithManager.
	RESTMapper apimeta.RESTMapper
	// Fetcher downloads and digest-verifies stage artifacts. Defaults to
	// artifact.New().
	Fetcher *artifact.Fetcher
	// Recorder emits Kubernetes Events (events.v1) on run/stage transitions;
	// nil disables event emission (tests that do not need events leave it
	// unset).
	Recorder events.EventRecorder

	// ShardCap is the global --inventory-shard-cap flag.
	ShardCap int
	// AllowedActionHosts is the global --allowed-action-hosts flag.
	AllowedActionHosts []string
	// ActionIPValidator pins each resolved action-URL address at dial time; nil
	// uses the production loopback/link-local/metadata denylist. Tests inject a
	// permissive validator so httptest loopback listeners stay reachable.
	ActionIPValidator func(net.IP) error
	// MetricIPValidator pins each resolved metric-source address at dial time;
	// nil uses the production loopback/link-local/metadata denylist (in-cluster
	// private ranges stay reachable). Tests inject a permissive validator.
	MetricIPValidator func(net.IP) error
	// MetricQuerier resolves a MetricSource to a scalar for the error-budget
	// freeze and promotion-analysis gates. Defaulted to a Prometheus querier in
	// SetupWithManager; tests substitute a fake so the gate logic is exercised
	// without a live Prometheus.
	MetricQuerier metricsource.Querier
	// NoCrossNamespaceRefs is the global --no-cross-namespace-refs flag.
	NoCrossNamespaceRefs bool
	// RequireVerifiedMigrationSources is the global
	// --require-verified-migration-sources flag: when true, a sourced migration
	// ladder runs only if its source carries SourceVerified=True. A source whose
	// verification FAILED is refused regardless of this flag.
	RequireVerifiedMigrationSources bool
	// RequirePinnedMigrationSources is the global
	// --require-pinned-migration-sources flag: when true, a sourced migration
	// ladder runs only if its source is pinned to an immutable revision
	// (digest/commit). When false, a mutable-pinned source runs but warns.
	RequirePinnedMigrationSources bool
	// ObjectLevelKMS is the global --object-level-kms flag: when true, SOPS
	// cloud KMS decryption uses the StageSet's serviceAccountName federated to
	// a cloud identity (object-level identity) instead of the controller's
	// ambient credentials. Default false keeps the ambient behavior so existing
	// setups are unaffected.
	ObjectLevelKMS bool
	// DefaultInterval is the global --default-interval flag: the reconcile
	// cadence used for StageSets that omit spec.interval.
	DefaultInterval time.Duration
	// MaxTeardownWait is the global --max-teardown-wait flag: how long a
	// deleting StageSet's finalizer holds while reverse-order teardown keeps
	// failing before reconcileDelete force-drops it (emitting a Warning
	// TeardownForced event and a metric, and possibly orphaning objects the
	// failing stage couldn't delete). Zero falls back to defaultMaxTeardownWait.
	MaxTeardownWait time.Duration
	// RollbackStore is the optional external store for rendered output, making
	// rollbackOnFailure bit-exact and independent of producer retention. Nil
	// falls back to re-fetching the producer artifact.
	RollbackStore RollbackStore
	// Now returns the current time for update-window evaluation; nil defaults
	// to time.Now. Tests inject a fixed clock.
	Now func() time.Time

	// remoteConfig builds the rest.Config for a spec.kubeConfig target (secretRef
	// kubeconfig or configMapRef cloud-provider auth). Defaulted to
	// defaultRemoteConfigBuilder in SetupWithManager; tests inject a fake that
	// points at envtest so the cloud path is exercised without a cloud account.
	remoteConfig remoteConfigBuilder

	// credentialSource overrides the per-tenant cloud KMS credential resolver
	// used when ObjectLevelKMS is enabled. Nil falls back to a
	// tenantCredentialSource backed by fluxcd/pkg/auth; tests inject a fake so
	// the object-level-KMS wiring is exercised without a cloud account.
	credentialSource decryptor.CredentialSource

	// targets memoizes the per-run target connection (client + RESTMapper) for
	// the impersonated and/or remote cluster a StageSet applies to; client.New
	// and remote discovery are costly to repeat each reconcile. Keyed by
	// cluster+SA+kubeconfig identity, guarded by tenantMu.
	tenantMu sync.Mutex
	targets  map[string]clusterTarget

	// controller is the built controller, kept (via Build instead of Complete)
	// so producer watches can be added dynamically — producer GVKs aren't known
	// until a StageSet references one. mgrCache constructs their source.Kind
	// sources. watchedProducers single-flights engagement per GVK.
	controller       controller.Controller
	mgrCache         cache.Cache
	watchMu          sync.Mutex
	watchedProducers map[schema.GroupVersionKind]struct{}
}

// +kubebuilder:rbac:groups=stages.metio.wtf,resources=stagesets,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=stages.metio.wtf,resources=stagesets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=stages.metio.wtf,resources=stagesets/finalizers,verbs=update
// +kubebuilder:rbac:groups=stages.metio.wtf,resources=stageinventories,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=stages.metio.wtf,resources=stageledgers,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=stages.metio.wtf,resources=stageledgers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=source.toolkit.fluxcd.io,resources=externalartifacts,verbs=get;list;watch
// Producer kinds whose failures the controller surfaces via dynamic watches:
// the Flux artifact-publishing sources and the JaaS snippet producer. A custom
// producer kind not listed here still works (resolution is via the EA
// back-pointer), just without the fast-failure watch unless its RBAC is added.
// +kubebuilder:rbac:groups=source.toolkit.fluxcd.io,resources=gitrepositories;ocirepositories;buckets,verbs=get;list;watch
// +kubebuilder:rbac:groups=jaas.metio.wtf,resources=jsonnetsnippets,verbs=get;list;watch
// Local-cluster apply assumes the tenant SA's identity by minting a
// short-lived TokenRequest token for it — no `impersonate` verb. The token
// authenticates as system:serviceaccount:<ns>:<sa>, so the tenant SA's RBAC
// bounds the apply. (Remote-cluster apply via spec.kubeConfig uses the
// provided kubeconfig and needs nothing here.)
// --object-level-kms resolves the tenant ServiceAccount (cloud workload
// identity annotations) through the cached client, so the SA read needs the
// informer verbs, not just get.
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=serviceaccounts/token,verbs=create
// +kubebuilder:rbac:groups=admissionregistration.k8s.io,resources=validatingwebhookconfigurations,verbs=get;update
// There is deliberately NO secrets/configmaps rule here. Every Secret and
// ConfigMap this controller reads is one a StageSet named in its own spec —
// postBuild substituteFrom, the metric-source bearer token, decryption keys,
// action secretRefs, spec.kubeConfig — and each resolves through the
// tenant-impersonated client, so the tenant SA's RBAC bounds it. Granting the
// controller a cluster-wide read would hand any StageSet author the contents of
// any Secret in their namespace, since they choose the names. It would also back
// those reads with cluster-wide Secret and ConfigMap informers, which is the
// bulk of a controller's cache. Single-tenant installs (rbac.clusterAdmin: true)
// reach these objects through cluster-admin instead; a multi-tenant StageSet
// without a serviceAccountName cannot apply anything anyway, so failing its read
// changes where it stops, not whether.

// Reconcile drives one StageSet through the design's state machine: resolve +
// pin artifacts, then BUILD -> APPLY -> PRUNE + RECORD -> VERIFY each stage in
// order; a finalizer tears the applied objects down on deletion.
func (r *StageSetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctx, span := observability.Tracer().Start(ctx, "StageSet.Reconcile",
		trace.WithAttributes(
			attribute.String("stageset.namespace", req.Namespace),
			attribute.String("stageset.name", req.Name),
		))
	defer span.End()

	// controller-runtime seeds this logger with namespace/name/reconcileID; the
	// logr->slog bridge turns those into structured JSON fields.
	logger := log.FromContext(ctx)

	var ss stagesv1.StageSet
	if err := r.Get(ctx, req.NamespacedName, &ss); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	span.SetAttributes(attribute.Int64("stageset.generation", ss.Generation))

	if !ss.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &ss)
	}

	// Snapshot the object before any status mutation so every status write
	// below can be issued through the Flux patch.Helper: the Ready condition is
	// patched via the helper's internal re-Get + optimistic-lock backoff loop,
	// so a Conflict (a sibling controller or a manual kubectl edit bumping
	// resourceVersion) is resolved by re-applying the condition diff to the
	// latest object rather than failing the whole reconcile. The plain status
	// fields merge-patch without a resourceVersion precondition, so they can't
	// conflict. The helper is created here, before the finalizer Update, so its
	// "before" snapshot reflects the persisted spec/metadata.
	patchHelper, err := fluxpatch.NewHelper(&ss, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}

	if controllerutil.AddFinalizer(&ss, FinalizerName) {
		if err := r.Update(ctx, &ss); err != nil {
			return ctrl.Result{}, err
		}
		// Adding a finalizer doesn't change metadata.generation, so the
		// resulting Update event is dropped by the GenerationChangedPredicate
		// on the For() watch. Requeue explicitly so the first real reconcile
		// runs instead of waiting for the interval (or an unrelated event).
		logger.V(1).Info("finalizer added; requeuing to reconcile")
		return ctrl.Result{Requeue: true}, nil
	}

	if ss.Spec.Suspend {
		r.setReady(&ss, metav1.ConditionFalse, ReasonSuspended, "Reconciliation is suspended")
		return ctrl.Result{}, r.patchStatus(ctx, patchHelper, &ss)
	}

	// Record that this run handled the current reconcile.fluxcd.io/requestedAt
	// token, so `flux reconcile`/kubectl-annotate force-reconciles can detect
	// completion. Stamped on the object now so every status write below
	// persists it, regardless of which path the run takes. (Suspended objects
	// are intentionally not stamped — the request was not acted on.)
	// A manual reconcile request (flux reconcile / annotation bump) clears a
	// MigrationDirty halt so a fixed migration is re-attempted once.
	if req := ss.Annotations[fluxmeta.ReconcileRequestAnnotation]; req != "" && req != ss.Status.GetLastHandledReconcileRequest() {
		ss.Status.MigrationFailureCount = 0
		ss.Status.ActionFailureCount = 0
	}
	ss.Status.SetLastHandledReconcileRequest(ss.Annotations[fluxmeta.ReconcileRequestAnnotation])

	// Spec invariants the CRD schema cannot express cheaply (the action oneof)
	// plus reserved post-v1 fields. The admission webhook normally rejects
	// these at write time; this is the fallback for a bypassed or disabled
	// webhook. Terminal: wait for a spec change rather than requeuing.
	if err := ValidateSpec(&ss); err != nil {
		r.setReady(&ss, metav1.ConditionFalse, ReasonInvalidSpec, err.Error())
		ss.Status.ObservedGeneration = ss.Generation
		if uerr := r.patchStatus(ctx, patchHelper, &ss); uerr != nil {
			return ctrl.Result{}, uerr
		}
		return ctrl.Result{RequeueAfter: permanentRetryInterval}, nil
	}

	// Dependency gating: every spec.dependsOn StageSet must be Ready at its
	// observed generation before this one runs. A dependsOn cycle is terminal.
	if ready, why, err := r.dependenciesReady(ctx, &ss); err != nil {
		return ctrl.Result{}, err
	} else if !ready {
		reason, terminal := ReasonDependencyNotReady, false
		if why == cycleSentinel {
			reason, terminal, why = ReasonStalled, true, "spec.dependsOn forms a cycle"
		}
		r.setReady(&ss, metav1.ConditionFalse, reason, why)
		ss.Status.ObservedGeneration = ss.Generation
		if uerr := r.patchStatus(ctx, patchHelper, &ss); uerr != nil {
			return ctrl.Result{}, uerr
		}
		if terminal {
			return ctrl.Result{RequeueAfter: permanentRetryInterval}, nil
		}
		return jitter.JitteredRequeueInterval(ctrl.Result{RequeueAfter: r.retryInterval(&ss)}), nil
	}

	// Producer-aware source resolution + revision pinning: resolve every
	// stage's ExternalArtifact (directly or through the RFC-0012 back-pointer)
	// and pin the run snapshot in status.lastAttemptedRevisions before anything
	// touches the cluster. Resolution always lands on an ExternalArtifact, so
	// the data plane stays single-kind.
	resolver := &artifact.Resolver{NoCrossNamespace: r.NoCrossNamespaceRefs}
	resolved := make([]artifact.ResolvedArtifact, len(ss.Spec.Stages))
	// Per-stage substitution fingerprint of this run, recorded in the rollback
	// snapshot so rollback can detect changed substituteFrom inputs.
	subDigests := make([]string, len(ss.Spec.Stages))
	pinned := make(map[string]string, len(ss.Spec.Stages))
	for i := range ss.Spec.Stages {
		// Dynamically watch a producer kind the first time it's referenced so its
		// failures surface here immediately, not at the next retryInterval.
		if ref := ss.Spec.Stages[i].SourceRef; isProducerRef(ref) {
			r.engageProducerWatch(producerGVK(ref))
		}
		ra, err := resolver.Resolve(ctx, r.graphReader(), ss.Spec.Stages[i].SourceRef, ss.Namespace)
		if err != nil {
			return r.failResolution(ctx, patchHelper, &ss, ss.Spec.Stages[i].Name, ss.Spec.Stages[i].SourceRef, ss.Namespace, err)
		}
		resolved[i] = ra
		pinned[ra.Key()] = ra.Revision
	}
	ss.Status.LastAttemptedRevisions = pinned

	// Time-based delivery: hold a new-revision rollout (or, under
	// windowScope=All, any reconcile) while update windows are closed, unless
	// a one-shot update-now override is present. A held-but-deployed StageSet
	// stays Ready; the held revisions and next window are surfaced on status.
	gateCtx, gateSpan := observability.Tracer().Start(ctx, "stageset.gateWindows")
	res, deferred, derr := r.gateUpdateWindows(gateCtx, patchHelper, &ss, resolved)
	if derr != nil {
		gateSpan.RecordError(derr)
		gateSpan.SetStatus(codes.Error, "update-window gating failed")
		gateSpan.End()
		return ctrl.Result{}, derr
	}
	gateSpan.SetAttributes(attribute.Bool("stageset.deferred", deferred))
	gateSpan.End()
	if deferred {
		logger.Info("rollout deferred while update windows are closed",
			"requeueAfter", res.RequeueAfter.String())
		return res, nil
	}

	// Budget break-glass: a fresh stages.metio.wtf/budget-override ships this run
	// once, past every error-budget freeze (rollout-wide and per-stage). Consume
	// it here so both gates see the same one-shot decision.
	budgetOverride := false
	if token, fresh := freshBudgetOverride(&ss); fresh {
		ss.Status.LastHandledBudgetOverride = token
		budgetOverride = true
	}

	// Error-budget freeze: hold a new-revision rollout while the service is out
	// of its SLO error budget. Combined with the update-window gate above; both
	// must allow. A frozen-but-deployed StageSet stays Ready with status.budgetFreeze.
	budgetCtx, budgetSpan := observability.Tracer().Start(ctx, "stageset.gateErrorBudget")
	budgetRes, budgetDeferred, berr := r.gateErrorBudget(budgetCtx, patchHelper, &ss, resolved, budgetOverride)
	if berr != nil {
		budgetSpan.RecordError(berr)
		budgetSpan.SetStatus(codes.Error, "error-budget gating failed")
		budgetSpan.End()
		return ctrl.Result{}, berr
	}
	budgetSpan.SetAttributes(attribute.Bool("stageset.budgetFrozen", budgetDeferred))
	budgetSpan.End()
	if budgetDeferred {
		logger.Info("rollout frozen by error budget", "requeueAfter", budgetRes.RequeueAfter.String())
		return budgetRes, nil
	}

	// Stage state machine: for each stage in order — run PRE actions, fetch +
	// BUILD the pinned artifact, APPLY (SSA), PRUNE + RECORD (StageInventory
	// diff), VERIFY (kstatus), then POST actions. Failures from APPLY onward run
	// onFailure best-effort. The action idempotency ledger lives in the stage
	// status, keyed by the pinned revision.
	// All cluster writes for this run — SSA apply, health-check reads, prune,
	// and the typed actions — go through the target connection: the remote
	// cluster when spec.kubeConfig is set, impersonating spec.serviceAccountName
	// when set, else the controller's own client. Bookkeeping (StageInventory,
	// status) always stays on the controller client and cluster.
	// Every per-stage cluster operation runs through a stageRuntime derived from
	// that stage's effective ServiceAccount (its own serviceAccountName, else the
	// StageSet default). runtimes memoizes one per SA across the whole reconcile —
	// the forward loop, the cross-stage prune, and any rollback — so stages
	// sharing an SA share a connection/token while distinct SAs stay RBAC-isolated.
	// The default-SA runtime is resolved up front: it validates spec.kubeConfig
	// (shared by every stage) once, and backs the cross-stage teardown of stages
	// removed from the spec, which no longer carry a per-stage SA.
	fetcher := r.fetcher()
	runtimes := map[string]*stageRuntime{}
	defaultRT, err := r.stageRuntime(ctx, &ss, ss.Spec.ServiceAccountName, fetcher, runtimes)
	if err != nil {
		// A malformed spec.kubeConfig (unknown cloud provider, missing required
		// ConfigMap key, unparseable kubeconfig Secret) is terminal: retrying
		// can't fix the spec, so surface it as InvalidSpec and wait for an edit
		// rather than burning reconciles. Transient connect failures still fail
		// the stage and back off.
		if errors.Is(err, errInvalidKubeConfigSpec) {
			prevReady := readyConditionSnapshot(&ss)
			r.setReady(&ss, metav1.ConditionFalse, ReasonInvalidSpec, err.Error())
			ss.Status.ObservedGeneration = ss.Generation
			metrics.ReconcileTotal.WithLabelValues(ss.Namespace, ss.Name, ReasonInvalidSpec).Inc()
			r.emitReadyEvent(&ss, prevReady, metav1.ConditionFalse, ReasonInvalidSpec, err.Error())
			if uerr := r.patchStatus(ctx, patchHelper, &ss); uerr != nil {
				return ctrl.Result{}, uerr
			}
			return ctrl.Result{RequeueAfter: permanentRetryInterval}, nil
		}
		return r.failStage(ctx, patchHelper, &ss, ss.Spec.Stages[0].Name, "connect to target cluster", err, nil, actionLedger{})
	}
	recorder := &stageinv.Recorder{Client: r.Client, ShardCap: r.ShardCap}
	// Versioned migrations: resolve the desired version and the migrations the
	// current transition crosses, before any stage runs. Terminal version
	// failures (InvalidVersion, downgrade) short-circuit here; the ordered
	// pending migrations are surfaced on status and run anchored to their
	// stages inside the loop.
	migCtx, migSpan := observability.Tracer().Start(ctx, "stageset.planMigrations")
	migPlan, mreason, mmsg, merr := r.planVersionMigrations(migCtx, &ss, resolved, fetcher)
	if merr != nil {
		migSpan.RecordError(merr)
		migSpan.SetStatus(codes.Error, "migration planning failed")
		migSpan.End()
		return ctrl.Result{}, merr // transient fetch
	}
	migSpan.End()
	if mreason != "" {
		r.setReady(&ss, metav1.ConditionFalse, mreason, mmsg)
		ss.Status.ObservedGeneration = ss.Generation
		if uerr := r.patchStatus(ctx, patchHelper, &ss); uerr != nil {
			return ctrl.Result{}, uerr
		}
		return ctrl.Result{RequeueAfter: permanentRetryInterval}, nil
	}
	ss.Status.PendingMigrations = migPlan.pendingDetails(&ss)

	// Approval gate: when spec.version.requireApproval is set and this transition
	// has pending migrations, hold the whole rollout (app and migrations) until an
	// operator approves the target version via the approved-version annotation.
	// pendingMigrations is already on status, so the operator sees exactly what
	// they are approving.
	if v := ss.Spec.Version; v != nil && v.RequireApproval && len(migPlan.pending) > 0 &&
		ss.Annotations[approvedVersionAnnotation] != migPlan.desired {
		prevReady := readyConditionSnapshot(&ss)
		msg := fmt.Sprintf("version transition to %s has %d pending migration(s) awaiting approval; set annotation %s=%q to proceed",
			migPlan.desired, len(migPlan.pending), approvedVersionAnnotation, migPlan.desired)
		r.setReady(&ss, metav1.ConditionFalse, ReasonMigrationApprovalPending, msg)
		ss.Status.ObservedGeneration = ss.Generation
		metrics.ReconcileTotal.WithLabelValues(ss.Namespace, ss.Name, ReasonMigrationApprovalPending).Inc()
		r.emitReadyEvent(&ss, prevReady, metav1.ConditionFalse, ReasonMigrationApprovalPending, msg)
		if uerr := r.patchStatus(ctx, patchHelper, &ss); uerr != nil {
			return ctrl.Result{}, uerr
		}
		return ctrl.Result{RequeueAfter: permanentRetryInterval}, nil
	}

	// SOPS decryptor (nil when spec.decryption is unset). Built once per
	// reconcile; the key Secret is read under the tenant SA.
	decCtx, decSpan := observability.Tracer().Start(ctx, "stageset.buildDecryptor")
	dec, derr := r.buildDecryptor(decCtx, &ss)
	if derr != nil {
		decSpan.RecordError(derr)
		decSpan.SetStatus(codes.Error, "decryptor configuration failed")
		decSpan.End()
		return r.failStage(ctx, patchHelper, &ss, ss.Spec.Stages[0].Name, "configure decryption", derr, nil, actionLedger{})
	}
	decSpan.End()

	priorStages := indexStageStatuses(ss.Status.Stages)

	// Single-stage force-reconcile: when the stages.metio.wtf/reconcile-stage
	// token is unhandled for a known stage, clear that stage's action ledger so
	// its pre/post actions re-run this pass, even though the pinned revision is
	// unchanged. Migration ledgers are NOT cleared here — completed migrations
	// stay completed across a force (only a version advance re-runs them); a
	// force with no version change has no newly-pending migrations anyway. The
	// token is recorded on the stage's status only on success (lastHandledFor),
	// so a forced stage that fails retries on the next reconcile.
	forceStage, forceToken := parseReconcileStage(&ss)
	if prior, ok := priorStages[forceStage]; forceStage == "" || !ok || prior.LastHandledReconcileAt == forceToken {
		forceStage, forceToken = "", ""
	} else {
		cleared := prior
		cleared.ExecutedActions = nil
		cleared.LedgerRevision = ""
		priorStages[forceStage] = cleared
	}

	// Version-ledger reset (stagesetctl reconcile --reset-scope=Version): the
	// deliberate "re-run the upgrade at the same version" escape hatch. Unlike a
	// force-reconcile (which clears the revision ledger), this clears every
	// stage's VERSION ledger, so scope: Version actions re-run once even though
	// the version is unchanged. One-shot, tracked by LastHandledResetScope.
	if token := ss.Annotations[resetScopeAnnotation]; token != "" && token != ss.Status.LastHandledResetScope {
		for name, prior := range priorStages {
			prior.ExecutedVersionActions = nil
			prior.LedgerVersion = ""
			priorStages[name] = prior
		}
		ss.Status.LastHandledResetScope = token
	}
	lastHandledFor := func(name string) string {
		if name == forceStage {
			return forceToken
		}
		return priorStages[name].LastHandledReconcileAt
	}

	// Load (or materialize) the once-per-lifetime ledger and reconcile its
	// spec.baseline assertions into recorded completions, before the stage loop
	// consults it to gate scope: Lifetime actions. nil when the spec has no
	// Lifetime action and no ledger exists — the common case, no extra objects.
	lifetimeLedger, llerr := r.loadLifetimeLedger(ctx, &ss)
	if llerr != nil {
		return ctrl.Result{}, llerr
	}
	// A StageSet on its first reconcile (no stage status yet) that adopts a ledger
	// already carrying completions is the retain-always surprise — a delete+recreate,
	// or a fresh StageSet over a retained ledger — where a recorded completion may
	// suppress an action that would otherwise run. Signal it once (the first
	// reconcile writes stage status, success or failure, so this cannot re-fire),
	// before baseline promotion adds more completions.
	if len(ss.Status.Stages) == 0 && lifetimeLedger != nil && len(lifetimeLedger.Status.CompletedActions) > 0 {
		metrics.LedgerAdoptionsTotal.WithLabelValues(ss.Namespace, ss.Name).Inc()
		r.event(&ss, corev1.EventTypeNormal, eventReasonLedgerAdopted,
			fmt.Sprintf("adopted an existing StageLedger with %d recorded completion(s); a scope: Lifetime action recorded complete will not run again (stagesetctl reset-ledger forgets one)", len(lifetimeLedger.Status.CompletedActions)))
	}
	if err := r.promoteBaseline(ctx, &ss, lifetimeLedger); err != nil {
		return ctrl.Result{}, err
	}

	previousMap, perr := recorder.StageRecords(ctx, ss.Name, ss.Namespace)
	if perr != nil {
		return ctrl.Result{}, perr
	}
	previousRecords := toInventoryRecords(previousMap)
	// A stage with no stored inventory that status records as previously applied
	// has lost its inventory (a stray delete, a partial restore) while its
	// objects are still live. Mark it for best-effort reconstruction from the
	// cluster during the apply loop; pruning is then deferred this pass.
	needsReconstruct := map[string]bool{}
	for i := range ss.Spec.Stages {
		name := ss.Spec.Stages[i].Name
		if _, ok := previousMap[name]; !ok && priorStages[name].AppliedRevision != "" {
			needsReconstruct[name] = true
		}
	}
	var reconstructedStages []string
	var partialReconstructStages []string
	desiredRecords := make([]inventory.StageRecord, 0, len(ss.Spec.Stages))
	applied := make(map[string]string, len(ss.Spec.Stages))
	stageStatuses := make([]stagesv1.StageStatus, 0, len(ss.Spec.Stages))
	// When a stage's promotion gate holds, these record which stage and its
	// phase so the post-loop hold handler can set the right Ready reason.
	var promoteHoldStage string
	var promoteHoldState *stagesv1.PromotionState
	// When a stage's own errorBudget holds a new-revision entry, these carry the
	// hold for the post-loop handler. stageBudgetFreeze records a dryRun
	// would-freeze to stamp on the stage status of a stage that still proceeds.
	var budgetHoldReason, budgetHoldMsg string
	// When a promotion gate's read is PERMANENTLY denied (RBAC), this carries
	// the operator-actionable message for the errGateDenied handler.
	var gateDeniedMsg string
	stageBudgetFreeze := map[string]*stagesv1.BudgetFreeze{}
	// The stage loop runs in a closure so a stage failure can be intercepted
	// for rollbackOnFailure before it is finalized; failStage's returns become
	// the closure's result.
	loopResult, loopErr := func() (ctrl.Result, error) {
		for i := range ss.Spec.Stages {
			stage := &ss.Spec.Stages[i]
			ra := resolved[i]

			// A prior promotion analysis with onFailure=Rollback reverted this
			// stage and aborted its failing revision. Stay reverted: skip
			// re-applying (and re-failing) that revision until it changes or an
			// operator promotes. Earlier stages already ran this pass; later stages
			// are not reached.
			if prior, ok := priorStages[stage.Name]; ok && rollbackAborted(&ss, stage, prior, ra.Revision) {
				stageStatuses = append(stageStatuses, prior)
				metrics.SetStagePromotionPending(ss.Namespace, ss.Name, stage.Name, true)
				metrics.SetStagePromotionBlocked(ss.Namespace, ss.Name, stage.Name, true)
				promoteHoldStage = stage.Name
				promoteHoldState = prior.PromotionState
				return ctrl.Result{RequeueAfter: r.retryInterval(&ss)}, errHoldForPromotion
			}

			// Idempotency ledger: carry actions already run for this pinned
			// revision; a new revision resets it. record appends in memory and is
			// persisted by failStage / the final stage status. priorRevision is the
			// revision this stage last applied, used to tell a content change apart
			// from out-of-band drift after the apply.
			// Two ledgers, keyed independently: the revision ledger resets on any
			// new revision (or force-reconcile), the version ledger only when the
			// resolved version changes — the per-stage episode rekey. Both carry
			// forward from prior status when their key still matches. record routes
			// each completed action into the ledger its scope selects.
			scopeOf := stageActionScopes(stage)
			led := actionLedger{revision: ra.Revision}
			if migPlan.versionSet {
				led.version = migPlan.desired
			}
			priorRevision := ""
			if prior, ok := priorStages[stage.Name]; ok {
				priorRevision = prior.AppliedRevision
				if prior.LedgerRevision == ra.Revision {
					led.executed = append(led.executed, prior.ExecutedActions...)
				}
				if migPlan.versionSet && prior.LedgerVersion == migPlan.desired {
					led.versioned = append(led.versioned, prior.ExecutedVersionActions...)
				}
			}
			// First-adoption baseline: when versioning records the version without
			// running migrations, Version-scoped actions baseline with it — an
			// existing fleet migrating into StageSet must not fire N maintenance
			// ladders. Harmless on a fresh install: episode one has nothing to
			// upgrade (bootstrap is scope: Lifetime's job, a later phase).
			if migPlan.baseline {
				led.versioned = append(led.versioned, versionScopedActionNames(stage)...)
			}
			// led.lifetimeDone and the record closure need the stage's effective-SA
			// client (to read completionAnchor witnesses), so they are wired below,
			// once rt is resolved.
			// A Version-scoped action carried over at a NEW revision is being held
			// off config churn — the feature working. Surface it once per revision
			// (only when the revision actually changed), not every reconcile.
			r.emitVersionSkips(&ss, stage, led, priorRevision, ra.Revision)

			// Per-stage error-budget freeze: gate ENTRY of a new revision into this
			// stage on the stage's own SLO budget, before anything is applied.
			// Holding parks the rollout here (earlier stages stay applied, this
			// stage keeps its current revision); a dryRun would-freeze is recorded
			// on the stage status but does not hold. The override skips it.
			if stage.ErrorBudget != nil && !budgetOverride && priorRevision != ra.Revision {
				g := r.gateStageBudget(ctx, &ss, stage, priorStages[stage.Name])
				switch {
				case g.specErr != nil:
					// Malformed thresholds: terminal, like other InvalidSpec fallbacks.
					r.setReady(&ss, metav1.ConditionFalse, ReasonInvalidSpec, fmt.Sprintf("stage %q errorBudget: %v", stage.Name, g.specErr))
					ss.Status.ObservedGeneration = ss.Generation
					return ctrl.Result{RequeueAfter: permanentRetryInterval}, r.patchStatus(ctx, patchHelper, &ss)
				case g.sourceErr != nil:
					// onSourceError=Allow: proceed, but surface the error (deduped).
					if prev := readyConditionSnapshot(&ss); prev == nil || prev.Reason != ReasonBudgetSourceUnavailable {
						r.event(&ss, corev1.EventTypeWarning, ReasonBudgetSourceUnavailable,
							fmt.Sprintf("stage %q error-budget source unavailable; proceeding (onSourceError=Allow): %v", stage.Name, g.sourceErr))
					}
				case g.hold:
					// Hold the new revision out of this stage. Keep the stage at its
					// prior applied state and record the freeze.
					held := priorStages[stage.Name]
					held.Name = stage.Name
					held.BudgetFreeze = g.freeze
					stageStatuses = append(stageStatuses, held)
					budgetHoldReason, budgetHoldMsg = g.reason, g.msg
					return ctrl.Result{RequeueAfter: g.requeue}, errHoldForBudget
				case g.freeze != nil:
					// dryRun would-freeze: stamp it on the status this stage builds
					// after it applies (below), without holding.
					stageBudgetFreeze[stage.Name] = g.freeze
				}
			}

			// Resolve the connection this stage's cluster operations run through:
			// its effective ServiceAccount (stage override, else the StageSet
			// default) against the target cluster. Stages sharing an SA reuse the
			// cached client/token; a per-stage SA isolates this stage's writes to
			// that ServiceAccount's RBAC. A connect failure fails this stage — a
			// token-mint hiccup is transient; a malformed kubeConfig was already
			// caught terminally by the default-runtime resolve before the loop.
			rt, rerr := r.stageRuntime(ctx, &ss, effectiveServiceAccount(&ss, stage), fetcher, runtimes)
			if rerr != nil {
				return r.failStage(ctx, patchHelper, &ss, stage.Name, "connect to target cluster", rerr, stageStatuses, led)
			}

			// Gate scope: Lifetime actions on the StageLedger, honoring
			// completionAnchor witnesses read under this stage's effective SA. An
			// anchored completion whose witness is gone (absent or a fresh UID) is
			// dropped so the action re-runs and re-records; an unreadable witness is
			// retained (fail open) and surfaced. Lifetime completions live in the
			// StageLedger, never in led (which stamps stage status).
			gate := r.evaluateLifetimeGate(ctx, rt.target, lifetimeLedger, ss.Namespace, stage.Name)
			led.lifetimeDone = gate.done
			for _, name := range gate.unreadable {
				metrics.LedgerAnchorErrorsTotal.WithLabelValues(ss.Namespace, ss.Name).Inc()
				r.event(&ss, corev1.EventTypeWarning, eventReasonLedgerAnchorUnreadable,
					fmt.Sprintf("stage %q action %q: completionAnchor unreadable; retaining the completion (grant the stage's ServiceAccount read on the anchor kind)", stage.Name, name))
			}
			if dropCompletions(lifetimeLedger, stage.Name, gate.invalidated) {
				if uerr := r.Status().Update(ctx, lifetimeLedger); uerr != nil {
					return r.failStage(ctx, patchHelper, &ss, stage.Name, "invalidate lifetime completion", uerr, stageStatuses, led)
				}
				for _, name := range gate.invalidated {
					r.event(&ss, corev1.EventTypeNormal, eventReasonLedgerInvalidated,
						fmt.Sprintf("stage %q action %q: completionAnchor witness is gone; the scope: Lifetime action will run again", stage.Name, name))
				}
			}

			anchorOf := stageAnchors(stage)
			baseRecord := led.recordFn(scopeOf)
			record := func(name string) error {
				// Count executions by scope (pre/post only; onFailure names are
				// absent from scopeOf and excluded). status carries the per-action
				// view; the metric stays scope-labeled to bound cardinality.
				if sc, ok := scopeOf[name]; ok {
					metrics.ActionRunsTotal.WithLabelValues(ss.Namespace, ss.Name, string(sc)).Inc()
				}
				// A Lifetime completion is persisted to the StageLedger immediately
				// (before the next action), witnessing its completionAnchor if any.
				if scopeOf[name] == stagesv1.ScopeLifetime {
					return r.recordLifetimeCompletion(ctx, rt.target, lifetimeLedger, ss.Namespace, stage.Name, name, anchorOf[name])
				}
				return baseRecord(name)
			}

			// Migrations anchored to this stage run before its pre-actions, so
			// version-conditional work (data conversions, immutable-object
			// recreation) happens before the stage applies its content.
			if merr := r.runStageMigrations(ctx, &ss, stage.Name, migPlan, rt.executor); merr != nil {
				ss.Status.MigrationFailureCount++
				op := opMigration
				if ss.Status.MigrationFailureCount >= maxMigrationFailures {
					// Repeated failures: stop auto-retrying destructive work against
					// an uncertain state. Cleared by a manual reconcile once fixed.
					op = opMigrationDirty
				}
				return r.failStage(ctx, patchHelper, &ss, stage.Name, op, merr, stageStatuses, led)
			}

			// PRE actions: before BUILD; a failure aborts the stage untouched
			// (nothing has been applied), so no onFailure runs.
			if stage.Actions != nil {
				if err := rt.executor.Run(ctx, ss.Namespace, stage.Actions.Pre, led.doneSet(), record); err != nil {
					return r.failStage(ctx, patchHelper, &ss, stage.Name, r.actionFailureOp(&ss, stage, gate, "pre-action"), err, stageStatuses, led)
				}
			}

			fetchCtx, fetchSpan := observability.Tracer().Start(ctx, "stage.fetch",
				trace.WithAttributes(attribute.String("stage", stage.Name)))
			fetchSpan.SetAttributes(
				attribute.String("stage.revision", ra.Revision),
				attribute.String("stage.digest", ra.Digest),
			)
			files, err := fetcher.Fetch(fetchCtx, ra.URL, ra.Digest, "")
			if err != nil {
				fetchSpan.RecordError(err)
				fetchSpan.SetStatus(codes.Error, "artifact fetch failed")
				fetchSpan.End()
				return r.failStage(ctx, patchHelper, &ss, stage.Name, "fetch artifact", err, stageStatuses, led)
			}
			fetchSpan.End()

			// decryptFiles takes no ctx; the span still times the SOPS pass.
			_, decryptSpan := observability.Tracer().Start(ctx, "stage.decrypt",
				trace.WithAttributes(attribute.String("stage", stage.Name)))
			files, err = decryptFiles(dec, files)
			if err != nil {
				decryptSpan.RecordError(err)
				decryptSpan.SetStatus(codes.Error, "decrypt failed")
				decryptSpan.End()
				return r.failStage(ctx, patchHelper, &ss, stage.Name, "decrypt", err, stageStatuses, led)
			}
			decryptSpan.End()
			vars, err := r.resolvePostBuildVars(ctx, &ss, stage.PostBuild)
			if err != nil {
				return r.failStage(ctx, patchHelper, &ss, stage.Name, "resolve postBuild variables", err, stageStatuses, led)
			}
			subDigests[i] = substitutionDigest(vars)
			// build.Build takes no ctx; the span still times the kustomize build.
			_, buildSpan := observability.Tracer().Start(ctx, "stage.build",
				trace.WithAttributes(attribute.String("stage", stage.Name)))
			objects, err := build.Build(files, build.Options{Path: stage.Path, Patches: stage.Patches}, vars)
			if err != nil {
				buildSpan.RecordError(err)
				buildSpan.SetStatus(codes.Error, "build failed")
				buildSpan.End()
				return r.failStage(ctx, patchHelper, &ss, stage.Name, "build", err, stageStatuses, led)
			}
			buildSpan.End()
			// Every applied object carries the per-stage discovery label, so
			// `kubectl get -l stages.metio.wtf/stage=<stage>` answers "what does
			// this stage own" with no project-specific tooling.
			apply.StampStageLabel(objects, stagesv1.StageLabel, stage.Name)
			conflicts, cerr := apply.ResolveConflictHandling(objects, stage, apply.NewForceToken())
			if cerr != nil {
				return r.failStage(ctx, patchHelper, &ss, stage.Name, "conflict policy", cerr, stageStatuses, led)
			}
			applyCtx, applySpan := observability.Tracer().Start(ctx, "stage.apply",
				trace.WithAttributes(
					attribute.String("stage", stage.Name),
					attribute.Int("stage.objectCount", len(objects)),
				))
			changeSet, err := rt.applier.Apply(applyCtx, ss.Name, ss.Namespace, objects, conflicts)
			if err != nil {
				applySpan.RecordError(err)
				applySpan.SetStatus(codes.Error, "apply failed")
				applySpan.End()
				r.runOnFailure(ctx, &ss, stage, rt.executor, led.doneSet(), record)
				return r.failStage(ctx, patchHelper, &ss, stage.Name, "apply", err, stageStatuses, led)
			}
			applySpan.End()
			r.reportDrift(&ss, stage, changeSet, priorRevision, ra.Revision)
			if r.RollbackStore != nil && ss.Spec.RollbackOnFailure {
				r.storeRendered(ctx, &ss, stage.Name, ra.Digest, objects)
			}

			// RECORD the applied set as the stage's inventory (write-ahead, before
			// VERIFY). Pruning is deferred to a single cross-stage pass after all
			// stages apply, so an object moved between stages transfers ownership
			// instead of being deleted then re-created.
			newRefs := make([]inventory.ObjectRef, 0, len(objects))
			for _, o := range objects {
				newRefs = append(newRefs, stageinv.RefOf(o))
			}
			desiredRecords = append(desiredRecords, inventory.StageRecord{Name: stage.Name, Position: i, Entries: newRefs})
			// On a lost inventory, fold the stage's still-live objects (found by
			// their owner + stage labels across the current render's GVKs) back
			// into the recorded set, so the next reconcile can prune what this
			// render no longer contains. The prune itself is deferred this pass
			// (below) — a best-effort rebuild never deletes on the same pass that
			// guessed the contents.
			writeRefs := newRefs
			if needsReconstruct[stage.Name] {
				// Reconstruction lists the applied objects, which live on the
				// target cluster (the remote cluster when spec.kubeConfig is set,
				// else the controller's own). The recorder's r.Client stays on the
				// controller cluster for the StageInventory shard read/write.
				recovered, rerr := recorder.ReconstructFromCluster(ctx, rt.target, ss.Name, ss.Namespace, stage.Name, objects)
				if rerr != nil {
					logger.Error(rerr, "stage inventory reconstruction was partial", "stage", stage.Name)
					partialReconstructStages = append(partialReconstructStages, stage.Name)
				}
				writeRefs = unionRefs(newRefs, recovered)
				reconstructedStages = append(reconstructedStages, stage.Name)
			}
			if werr := recorder.Write(ctx, &ss, stage.Name, i, writeRefs); werr != nil {
				r.runOnFailure(ctx, &ss, stage, rt.executor, led.doneSet(), record)
				return r.failStage(ctx, patchHelper, &ss, stage.Name, "record inventory", werr, stageStatuses, led)
			}

			// Verify readiness. The kstatus wait covers the stage's applied
			// objects (unless DisableWait) plus any explicit ReadyChecks.Checks;
			// CEL ReadyChecks.Exprs then gate on the live state of matching
			// applied objects. All three share the stage's verify timeout.
			verifyTimeout := stageTimeout(&ss, stage)
			waitSet := readyCheckObjects(rt.mapper, &ss, stage)
			if !disableWait(stage) {
				waitSet = append(changeSet.ToObjMetadataSet(), waitSet...)
			}
			if len(waitSet) > 0 {
				if err := rt.applier.Wait(ctx, waitSet, verifyTimeout); err != nil {
					r.runOnFailure(ctx, &ss, stage, rt.executor, led.doneSet(), record)
					return r.failStage(ctx, patchHelper, &ss, stage.Name, "verify", err, stageStatuses, led)
				}
			}
			if err := evalReadyExprs(ctx, rt.target, &ss, stage, objects, verifyTimeout); err != nil {
				r.runOnFailure(ctx, &ss, stage, rt.executor, led.doneSet(), record)
				return r.failStage(ctx, patchHelper, &ss, stage.Name, "verify", err, stageStatuses, led)
			}

			// POST actions: the stage (and any downstream gate) is Ready only once
			// these succeed.
			if stage.Actions != nil {
				if err := rt.executor.Run(ctx, ss.Namespace, stage.Actions.Post, led.doneSet(), record); err != nil {
					r.runOnFailure(ctx, &ss, stage, rt.executor, led.doneSet(), record)
					return r.failStage(ctx, patchHelper, &ss, stage.Name, r.actionFailureOp(&ss, stage, gate, "post-action"), err, stageStatuses, led)
				}
			}

			// PROMOTE-GATE: the stage is applied, verified, and its post-actions
			// ran. Decide whether the rollout may advance past it (soak / analysis
			// / manual). Holding parks the rollout here — the stage stays applied
			// (drift is still corrected on the next reconcile) and later stages are
			// never touched — so this is a hold, not a failure.

			// recordGateInterrupted records the in-flight stage before a
			// gate-unevaluable exit: the stage applied, verified, and ran its
			// actions this pass, so its action ledger (and the handled
			// force-reconcile token) must persist — otherwise every backoff retry
			// re-runs the actions, and an unhandled force token would clear the
			// ledger again on the next pass. Promotion fields stay at their prior
			// values: no final gate verdict exists, so a pending promote token must
			// remain pending and the soak clock untouched.
			recordGateInterrupted := func() {
				prior := priorStages[stage.Name]
				stageStatuses = append(stageStatuses, led.stamp(stagesv1.StageStatus{
					Name:                   stage.Name,
					Phase:                  stagesv1.StageReady,
					AppliedRevision:        ra.Revision,
					EntriesCount:           int64(len(newRefs)),
					LastHandledReconcileAt: lastHandledFor(stage.Name),
					PromotionState:         prior.PromotionState,
					LastHandledPromotion:   prior.LastHandledPromotion,
					BudgetFreeze:           stageBudgetFreeze[stage.Name],
				}))
			}
			var verdict *analysisVerdict
			if stage.Promotion != nil && stage.Promotion.Analysis != nil {
				v := r.evaluatePromotionAnalysis(ctx, &ss, stage)
				verdict = &v
			}
			fastTrackOK := false
			if stage.Promotion != nil && stage.Promotion.FastTrack != nil {
				fastTrackOK = r.evaluateFastTrack(ctx, &ss, stage)
			}
			var restart *restartVerdict
			if stage.Promotion != nil && stage.Promotion.RestartGate != nil {
				rv, rerr := r.evaluateRestartChecks(ctx, r.gateReader(rt.target), &ss, stage)
				if rerr != nil {
					// Can't read the watched pods: never advance blind — the gate
					// fails closed. The stage applied successfully, so this is not
					// a stage failure: the sentinels keep the post-loop handler
					// from rolling back. A PERMANENT apiserver denial (Forbidden,
					// missing kind) won't heal by backoff — surface it on the
					// Ready condition and requeue at the bounded interval so a
					// granted verb self-heals; anything else backs off and retries.
					recordGateInterrupted()
					if isPermanentAPIError(rerr) {
						gateDeniedMsg = rbacDenialMessage(fmt.Sprintf("reading pods for stage %q restart gate", stage.Name), rerr)
						return ctrl.Result{RequeueAfter: permanentRetryInterval}, errGateDenied
					}
					return ctrl.Result{}, fmt.Errorf("%w: %w", errGateUnevaluable, rerr)
				}
				restart = rv
			}
			var event *eventVerdict
			if stage.Promotion != nil && stage.Promotion.EventGate != nil {
				ev, eerr := r.evaluateEventChecks(ctx, r.gateReader(rt.target), &ss, stage)
				if eerr != nil {
					// Can't read events: fail closed. Not a stage failure — the
					// sentinels keep the handler from rolling back. Permanent
					// denials surface on Ready and requeue bounded, like the
					// restart gate above; transients back off and retry.
					recordGateInterrupted()
					if isPermanentAPIError(eerr) {
						gateDeniedMsg = rbacDenialMessage(fmt.Sprintf("reading events for stage %q event gate", stage.Name), eerr)
						return ctrl.Result{RequeueAfter: permanentRetryInterval}, errGateDenied
					}
					return ctrl.Result{}, fmt.Errorf("%w: %w", errGateUnevaluable, eerr)
				}
				event = ev
			}
			promoted, promoState, promoteRequeue, handledPromotion, rollback := r.gatePromotion(&ss, stage, ra.Revision, priorStages[stage.Name], r.now(), verdict, fastTrackOK, restart, event)
			appliedRevision := ra.Revision
			if rollback {
				// A promotion gate with onFailure=Rollback (analysis or a restart
				// check) reverts THIS stage to its last-good revision — earlier
				// promoted stages are untouched — and records the failing revision as
				// aborted so it is not re-applied and re-failed each reconcile until
				// the revision changes or an operator promotes.
				cause := "promotion analysis"
				if promoState != nil && promoState.RestartCheck != "" {
					cause = fmt.Sprintf("restart check %q", promoState.RestartCheck)
				} else if promoState != nil && promoState.EventCheck != "" {
					cause = fmt.Sprintf("event check %q", promoState.EventCheck)
				}
				reverted, ok, rerr := r.rollbackStageToSnapshot(ctx, &ss, stage, i, rt.applier, fetcher, recorder)
				if rerr != nil {
					// Transient revert failure: back off. Not a stage failure — the
					// sentinel keeps the handler from also running attemptRollback.
					// The revert didn't land, so the gate verdict was not acted on:
					// record the stage with its prior promotion fields, like the
					// unevaluable-read exits above, and let the retry re-judge.
					recordGateInterrupted()
					return ctrl.Result{}, fmt.Errorf("%w: %w", errGateUnevaluable, rerr)
				}
				if ok {
					appliedRevision = reverted
					promoState.AbortedRevision = ra.Revision
					r.event(&ss, corev1.EventTypeWarning, ReasonPromotionBlocked,
						fmt.Sprintf("stage %q %s failed; rolled back to revision %s", stage.Name, cause, reverted))
					// The stage's manifests are restored; run post-rollback cleanup.
					r.runOnRollback(ctx, &ss, fetcher, runtimes)
				} else {
					r.event(&ss, corev1.EventTypeWarning, ReasonPromotionBlocked,
						fmt.Sprintf("stage %q %s failed and onFailure=Rollback, but no last-good revision is available to roll back to (enable spec.rollbackOnFailure); holding", stage.Name, cause))
				}
			}
			applied[ra.Key()] = ra.Revision
			stageStatuses = append(stageStatuses, led.stamp(stagesv1.StageStatus{
				Name:                   stage.Name,
				Phase:                  stagesv1.StageReady,
				AppliedRevision:        appliedRevision,
				EntriesCount:           int64(len(newRefs)),
				LastHandledReconcileAt: lastHandledFor(stage.Name),
				PromotionState:         promoState,
				LastHandledPromotion:   handledPromotion,
				BudgetFreeze:           stageBudgetFreeze[stage.Name],
			}))
			metrics.StageAppliedTotal.WithLabelValues(ss.Namespace, ss.Name, stage.Name).Inc()
			metrics.SetStagePromotionPending(ss.Namespace, ss.Name, stage.Name, !promoted)
			metrics.SetStagePromotionBlocked(ss.Namespace, ss.Name, stage.Name, promoState != nil && promoState.Phase == stagesv1.PromotionBlocked)
			if !promoted {
				promoteHoldStage = stage.Name
				promoteHoldState = promoState
				return ctrl.Result{RequeueAfter: promoteRequeue}, errHoldForPromotion
			}
		}
		return ctrl.Result{}, nil
	}()
	if errors.Is(loopErr, errHoldForPromotion) {
		// A promotion gate is holding the rollout at promoteHoldStage. Persist the
		// stage statuses (so the soak clock + handled token survive), set the held
		// Ready condition, and requeue. Earlier stages stay applied; later stages
		// were never processed, so the merge keeps their prior records.
		ss.Status.Stages = mergeStageStatuses(&ss, stageStatuses)
		publishStageReady(&ss)
		ss.Status.ObservedGeneration = ss.Generation
		reason, msg := promotionHoldCondition(&ss, promoteHoldStage, promoteHoldState)
		prevReady := readyConditionSnapshot(&ss)
		r.setReady(&ss, metav1.ConditionFalse, reason, msg)
		r.emitReadyEvent(&ss, prevReady, metav1.ConditionFalse, reason, msg)
		return loopResult, r.patchStatus(ctx, patchHelper, &ss)
	}
	if errors.Is(loopErr, errHoldForBudget) {
		// A stage's own errorBudget is holding a new revision out of that stage.
		// Persist the stage statuses (merged, so unprocessed later stages keep
		// their records), set the held Ready condition (a deployed StageSet stays
		// Ready=True — its current state is healthy), and requeue.
		ss.Status.Stages = mergeStageStatuses(&ss, stageStatuses)
		publishStageReady(&ss)
		ss.Status.ObservedGeneration = ss.Generation
		prevReady := readyConditionSnapshot(&ss)
		if len(ss.Status.LastAppliedRevisions) > 0 {
			r.setReady(&ss, metav1.ConditionTrue, ReasonReady, "Deployed; "+budgetHoldMsg)
			r.emitReadyEvent(&ss, prevReady, metav1.ConditionTrue, ReasonReady, budgetHoldMsg)
		} else {
			r.setReady(&ss, metav1.ConditionFalse, budgetHoldReason, budgetHoldMsg)
			r.emitReadyEvent(&ss, prevReady, metav1.ConditionFalse, budgetHoldReason, budgetHoldMsg)
		}
		return loopResult, r.patchStatus(ctx, patchHelper, &ss)
	}
	if errors.Is(loopErr, errGateUnevaluable) {
		// A promotion gate could not be evaluated this pass (a transient read
		// error on the watched pods/events, or a transient revert failure). The
		// current stage applied successfully, so this is not a stage failure: back
		// off and retry without rolling the healthy rollout back.
		//
		// The pass's progress MUST persist before the retry: the stage action
		// ledgers (recordGateInterrupted appended the in-flight stage), the
		// migration ledgers runStageMigrations mutated on ss.Status in memory, and
		// the handled force-reconcile token. Without this write every backoff
		// retry re-runs completed actions — including destructive migration
		// actions the ledger exists to run exactly once. The Ready condition and
		// observedGeneration stay untouched: the rollout state didn't change, a
		// transient blip must not flap them.
		ss.Status.Stages = mergeStageStatuses(&ss, stageStatuses)
		publishStageReady(&ss)
		if perr := r.patchStatus(ctx, patchHelper, &ss); perr != nil {
			return loopResult, errors.Join(loopErr, perr)
		}
		return loopResult, loopErr
	}
	if errors.Is(loopErr, errGateDenied) {
		// A promotion gate's read is PERMANENTLY denied (the tenant SA lost
		// pods/events read, a kind is missing). Backoff can't heal it and
		// silence would leave the last-written condition — often Ready=True —
		// on a rollout that is actually wedged. Persist the pass's progress,
		// surface the denial on Ready so the operator sees what verb to grant,
		// and requeue at the bounded interval so the granted verb self-heals.
		ss.Status.Stages = mergeStageStatuses(&ss, stageStatuses)
		publishStageReady(&ss)
		ss.Status.ObservedGeneration = ss.Generation
		prevReady := readyConditionSnapshot(&ss)
		r.setReady(&ss, metav1.ConditionFalse, ReasonRBACDenied, gateDeniedMsg)
		r.emitReadyEvent(&ss, prevReady, metav1.ConditionFalse, ReasonRBACDenied, gateDeniedMsg)
		return loopResult, r.patchStatus(ctx, patchHelper, &ss)
	}
	if loopErr != nil {
		// A stage failed. failStage has already written the failure status. If
		// rollbackOnFailure is set, restore the last-good snapshot; a snapshot
		// no longer fetchable surfaces as a terminal PreviousRevisionUnavailable.
		if ss.Spec.RollbackOnFailure {
			rbCtx, rbSpan := observability.Tracer().Start(ctx, "stageset.rollback")
			rbReason, rbMsg, rbErr := r.attemptRollback(rbCtx, &ss, runtimes, fetcher, recorder)
			if rbErr != nil {
				// Transient rollback failure (store outage, apiserver blip).
				// The stage failure status is already written; back off and
				// retry rather than masking it as terminal. The original
				// loopErr still drives the requeue below if it's the stronger
				// signal, but surfacing rbErr keeps the rollback-store outage
				// visible in logs.
				rbSpan.RecordError(rbErr)
				rbSpan.SetStatus(codes.Error, "rollback transient failure")
				rbSpan.End()
				logger.Error(rbErr, "rollback deferred by a transient failure; backing off")
				return ctrl.Result{}, rbErr
			}
			if rbReason != "" {
				rbSpan.SetStatus(codes.Error, rbReason)
				rbSpan.End()
				r.setReady(&ss, metav1.ConditionFalse, rbReason, rbMsg)
				ss.Status.ObservedGeneration = ss.Generation
				return ctrl.Result{}, r.patchStatus(ctx, patchHelper, &ss)
			}
			rbSpan.End()
			r.event(&ss, corev1.EventTypeWarning, eventReasonRolledBack,
				"rolled back to the last-applied revisions after a failed run")
			logger.Info("rolled back to the last-applied revisions after a failed run")
			// Post-rollback cleanup runs only when a snapshot was actually
			// restored; an empty snapshot means nothing rolled back, so there is
			// nothing to clean up.
			if len(ss.Status.LastAppliedSnapshot) > 0 {
				r.runOnRollback(rbCtx, &ss, fetcher, runtimes)
			}
		}
		// A terminal stage failure (RBAC denial, digest mismatch, oversized
		// tarball) halts the run but must not requeue — the failure status is
		// already written, and retry can't fix the cause. Return nil so
		// controller-runtime waits for the next genuine watch event or interval.
		if errors.Is(loopErr, errTerminalStageFailure) {
			return ctrl.Result{}, nil
		}
		return loopResult, loopErr
	}

	// Cross-stage prune: diff the previous inventory against this run's with
	// ownership transfer — an object moved to another stage is kept, an object
	// gone from every stage is pruned (honoring stage.prune), and stages removed
	// from the spec are torn down in reverse recorded order. A single object
	// claimed by two stages is an ambiguous spec and stalls the run.
	if dups := inventory.DuplicateClaims(desiredRecords); len(dups) > 0 {
		r.setReady(&ss, metav1.ConditionFalse, ReasonInvalidSpec,
			fmt.Sprintf("%d object(s) are claimed by more than one stage", len(dups)))
		ss.Status.ObservedGeneration = ss.Generation
		return ctrl.Result{}, r.patchStatus(ctx, patchHelper, &ss)
	}
	plan := inventory.ComputePlan(previousRecords, desiredRecords)
	if len(reconstructedStages) > 0 {
		// A stage's inventory was rebuilt from the live cluster this pass. Defer
		// all pruning and teardown to the next reconcile — when the inventory is
		// authoritative again — rather than deleting against a best-effort
		// reconstruction. The operator-visible event marks the recovery.
		msg := fmt.Sprintf("rebuilt StageInventory for stage(s) %s from live cluster objects; prune deferred to the next reconcile",
			strings.Join(reconstructedStages, ", "))
		if len(partialReconstructStages) > 0 {
			// A partial rebuild means some GVKs could not be listed, so the
			// rebuilt set may miss live objects — the deferred prune could later
			// orphan them. Surface that so an operator can investigate rather than
			// trusting the rebuild as complete.
			msg += fmt.Sprintf("; reconstruction was INCOMPLETE for stage(s) %s (some objects may be missing from the rebuilt inventory — check the controller log)",
				strings.Join(partialReconstructStages, ", "))
		}
		r.event(&ss, corev1.EventTypeWarning, inventoryReconstructedEvent, msg)
		logger.Info("StageInventory reconstructed from cluster; prune deferred to next reconcile",
			"stages", reconstructedStages, "partial", partialReconstructStages)
	} else {
		prunes := stagePruneByName(&ss)
		specStages := make(map[string]*stagesv1.Stage, len(ss.Spec.Stages))
		for i := range ss.Spec.Stages {
			specStages[ss.Spec.Stages[i].Name] = &ss.Spec.Stages[i]
		}
		for stageName, refs := range plan.PrunePerStage {
			if allowed, known := prunes[stageName]; known && !allowed {
				continue
			}
			if len(refs) == 0 {
				continue
			}
			// Garbage-collect a stage's dropped objects under the same identity
			// that applied them — its effective ServiceAccount — so a per-stage SA
			// with create rights can also prune. A stage that produced a prune set
			// is still in the spec, so it resolves; defaultRT is a safety fallback.
			pruneApplier := defaultRT.applier
			if st, ok := specStages[stageName]; ok {
				prt, perr := r.stageRuntime(ctx, &ss, effectiveServiceAccount(&ss, st), fetcher, runtimes)
				if perr != nil {
					return ctrl.Result{}, perr
				}
				pruneApplier = prt.applier
			}
			if _, derr := pruneApplier.Delete(ctx, ss.Name, ss.Namespace, stageinv.Objects(refs)); derr != nil {
				return ctrl.Result{}, derr
			}
		}
		for _, removed := range plan.RemovedStages {
			// A stage removed from the spec no longer carries a per-stage SA, so its
			// teardown runs under the StageSet-level default identity.
			if len(removed.Entries) > 0 {
				if _, derr := defaultRT.applier.Delete(ctx, ss.Name, ss.Namespace, stageinv.Objects(removed.Entries)); derr != nil {
					return ctrl.Result{}, derr
				}
			}
			if derr := recorder.DeleteStageShards(ctx, ss.Namespace, ss.Name, removed.Name); derr != nil {
				return ctrl.Result{}, derr
			}
			// Drop the removed stage's readiness gauge so a metric-based rollout
			// gate doesn't keep reading its last value for a stage that no longer
			// exists. publishStageReady only refreshes current status.Stages.
			metrics.DeleteStageReadyForStage(ss.Namespace, ss.Name, removed.Name)
		}
	}

	ss.Status.LastAppliedRevisions = applied
	ss.Status.Stages = stageStatuses
	publishStageReady(&ss)
	ss.Status.ObservedGeneration = ss.Generation
	// A fully successful run advances the recorded version and clears the
	// in-flight migration ledger (baselining records the version, having run
	// no migrations).
	if migPlan.versionSet {
		if migPlan.baseline {
			r.event(&ss, corev1.EventTypeNormal, eventReasonBaselined,
				fmt.Sprintf("baselined at %s on first adoption: no migrations were run; the deployment is assumed already at this version", migPlan.desired))
		}
		ss.Status.Version = migPlan.desired
		ss.Status.ExecutedMigrations = nil
		ss.Status.ExecutedMigrationActions = nil
		ss.Status.PendingMigrations = nil
		ss.Status.MigrationFailureCount = 0
	}
	// Record this run as the rollback target: per-stage artifact pointers in
	// status (no rendered output, no Secret). The status update below persists
	// it. When an external rollback store is configured, also push the
	// bit-exact rendered output for GC-independent rollback.
	if ss.Spec.RollbackOnFailure {
		ss.Status.LastAppliedSnapshot = snapshotStages(&ss, resolved, subDigests)
	}
	// A reconcile that applied and verified every stage clears the action-failure
	// counter: whatever was retrying (including a scope: Lifetime bootstrap) has
	// succeeded, so a future failure starts its escalation from zero.
	ss.Status.ActionFailureCount = 0
	syncedMsg := fmt.Sprintf("Applied and verified %d stage(s)", len(ss.Spec.Stages))
	prevReady := readyConditionSnapshot(&ss)
	r.setReady(&ss, metav1.ConditionTrue, ReasonReady, syncedMsg)
	if err := r.patchStatus(ctx, patchHelper, &ss); err != nil {
		return ctrl.Result{}, err
	}
	metrics.ReconcileTotal.WithLabelValues(ss.Namespace, ss.Name, ReasonReady).Inc()
	r.emitReadyEvent(&ss, prevReady, metav1.ConditionTrue, ReasonReady, syncedMsg)
	logger.Info("StageSet synced", "stages", len(ss.Spec.Stages), "ready", true)

	return jitter.JitteredRequeueInterval(ctrl.Result{RequeueAfter: r.steadyInterval(&ss)}), nil
}

// failResolution records the resolution failure on the Ready condition and
// chooses a requeue strategy: transient waits (artifact not yet present or not
// Ready) requeue at RetryInterval; terminal spec/config and permanent-API
// errors (RBAC denial, missing CRD) requeue at the bounded
// permanentRetryInterval so an out-of-band fix (granted RBAC, installed CRD)
// self-heals rather than burning reconciles; genuinely transient API errors are
// returned so controller-runtime backs off.
//
// When the failing ref targets another namespace, the message is scrubbed to a
// single constant so a tenant cannot distinguish NotFound / Forbidden / digest
// mismatch / 5xx about a namespace they don't own. Same-namespace failures stay
// verbatim.
func (r *StageSetReconciler) failResolution(ctx context.Context, helper *fluxpatch.Helper, ss *stagesv1.StageSet, stage string, ref stagesv1.SourceReference, ownerNS string, err error) (ctrl.Result, error) {
	var (
		reason    = ReasonResolveFailed
		transient bool
		apiError  bool
	)
	switch {
	case isPermanentAPIError(err):
		// RBAC denial / missing CRD / schema rejection while reading the source
		// CR. Non-recoverable by retry — terminal, not backoff.
		reason = ReasonRBACDenied
	case errors.Is(err, artifact.ErrSourceNotReady):
		reason, transient = ReasonSourceNotReady, true
	case errors.Is(err, artifact.ErrArtifactNotFound), errors.Is(err, artifact.ErrArtifactMissing):
		reason, transient = ReasonArtifactNotFound, true
	case errors.Is(err, artifact.ErrProducerAPIVersionRequired):
		// A spec problem: the reference can never resolve until the author
		// adds the apiVersion the docs already require.
		reason = ReasonInvalidSpec
	case errors.Is(err, artifact.ErrAmbiguousProducer), errors.Is(err, artifact.ErrCrossNamespaceForbidden):
		reason = ReasonResolveFailed
	default:
		// Unexpected (transient API/list/get failure): report and back off.
		reason, apiError = ReasonResolveFailed, true
	}

	var msg string
	switch {
	case isCrossNamespaceRef(ref, ownerNS):
		// Cross-namespace: collapse every failure mode to one constant so the
		// reachability of another namespace's source CRs can't be probed.
		msg = scrubbedCrossNamespaceMessage(ref, ref.Namespace)
	case reason == ReasonRBACDenied:
		msg = fmt.Sprintf("stage %q: %s", stage, rbacDenialMessage("resolving the source CR", err))
	default:
		msg = fmt.Sprintf("stage %q: %v", stage, err)
	}

	r.setReady(ss, metav1.ConditionFalse, reason, msg)
	ss.Status.ObservedGeneration = ss.Generation
	if uerr := r.patchStatus(ctx, helper, ss); uerr != nil {
		return ctrl.Result{}, uerr
	}
	switch {
	case apiError:
		return ctrl.Result{}, err
	case transient:
		return jitter.JitteredRequeueInterval(ctrl.Result{RequeueAfter: r.retryInterval(ss)}), nil
	default:
		// Terminal spec/config resolve failure (ambiguous producer,
		// cross-namespace rejected). No error, so controller-runtime doesn't
		// back off; a bounded RequeueAfter re-checks so a fix made elsewhere
		// (the second producer removed, RBAC granted on the source) heals
		// within the interval without a watch event.
		return ctrl.Result{RequeueAfter: permanentRetryInterval}, nil
	}
}

func (r *StageSetReconciler) setReady(ss *stagesv1.StageSet, status metav1.ConditionStatus, reason, message string) {
	// fluxconditions.Set stamps the condition's ObservedGeneration from the
	// object's generation and preserves LastTransitionTime when only the
	// message changes — same surface as apimeta.SetStatusCondition, but the
	// resulting condition diff is what the patch.Helper's conflict-safe
	// patchStatusConditions loop applies.
	fluxconditions.Set(ss, &metav1.Condition{
		Type:    ConditionReady,
		Status:  status,
		Reason:  reason,
		Message: r.decorateMessage(reason, message),
	})
}

// readyConditionSnapshot returns a value copy of the current Ready condition, or
// nil if absent. It must be called BEFORE setReady: fluxconditions.Set updates
// the condition in place (apimeta.FindStatusCondition hands back a pointer into
// the slice), so emitReadyEvent's dedup needs this prior snapshot rather than
// the just-written state.
func readyConditionSnapshot(ss *stagesv1.StageSet) *metav1.Condition {
	if cur := apimeta.FindStatusCondition(ss.Status.Conditions, ConditionReady); cur != nil {
		snapshot := *cur
		return &snapshot
	}
	return nil
}

// emitReadyEvent records a Kubernetes event for a Ready-condition transition,
// deduplicated against prev: it fires only when the Ready status or reason
// actually changes. A StageSet re-reconciling at its steady interval — or
// retrying a terminal failure every permanentRetryInterval — therefore doesn't
// re-emit an identical event on every pass. The event type follows the status
// (True -> Normal, otherwise Warning). prev comes from readyConditionSnapshot,
// taken before setReady mutated the condition. Mirrors the jaas operator's
// emitConditionEvent.
func (r *StageSetReconciler) emitReadyEvent(ss *stagesv1.StageSet, prev *metav1.Condition, status metav1.ConditionStatus, reason, message string) {
	if prev != nil && prev.Status == status && prev.Reason == reason {
		return
	}
	eventtype := corev1.EventTypeWarning
	if status == metav1.ConditionTrue {
		eventtype = corev1.EventTypeNormal
	}
	r.event(ss, eventtype, reason, message)
}

// patchStatus persists accumulated status changes (the Ready condition plus the
// plain status fields) through the per-reconcile patch.Helper. The Ready
// condition goes through the helper's optimistic-lock retry loop so a sibling
// controller bumping resourceVersion is resolved by re-applying the condition
// diff rather than failing the reconcile.
func (r *StageSetReconciler) patchStatus(ctx context.Context, helper *fluxpatch.Helper, ss *stagesv1.StageSet) error {
	return helper.Patch(ctx, ss, fluxpatch.WithOwnedConditions{Conditions: []string{ConditionReady}})
}

// happyReasonsNoRunbook names Ready reasons describing a healthy or
// intentionally-operator-set state, which therefore carry no runbook link:
// there is nothing to remediate. They still have a runbook page (the drift gate
// requires one) — it just documents that the state is expected.
var happyReasonsNoRunbook = map[string]bool{
	ReasonReady:     true,
	ReasonSuspended: true,
}

// RunbookBaseURL is the documentation site's runbook directory. decorateMessage
// appends a per-reason link under it so kubectl describe surfaces a direct route
// to the remediation page. Exported so other surfaces (the MCP server) build the
// same links without importing the controller package's heavier internals.
const RunbookBaseURL = "https://stageset.projects.metio.wtf/runbooks/"

// runbookBaseURL is the unexported alias used internally.
const runbookBaseURL = RunbookBaseURL

// decorateMessage appends a "(runbook: <base><reason>/)" suffix so kubectl
// describe surfaces a direct link to the per-reason remediation page on the
// documentation site. The reason is lower-cased into a path segment matching the
// Hugo page URL. Happy reasons get no suffix.
func (r *StageSetReconciler) decorateMessage(reason, message string) string {
	if happyReasonsNoRunbook[reason] {
		return message
	}
	return message + " (runbook: " + runbookBaseURL + strings.ToLower(reason) + "/)"
}

// effectiveInterval is the StageSet's reconcile cadence: spec.interval when set,
// otherwise the controller-wide --default-interval. spec.interval is optional so
// most StageSets can omit it and inherit the cluster default.
func (r *StageSetReconciler) effectiveInterval(ss *stagesv1.StageSet) time.Duration {
	if ss.Spec.Interval.Duration > 0 {
		return ss.Spec.Interval.Duration
	}
	return r.DefaultInterval
}

func (r *StageSetReconciler) retryInterval(ss *stagesv1.StageSet) time.Duration {
	if ss.Spec.RetryInterval != nil {
		return ss.Spec.RetryInterval.Duration
	}
	return r.effectiveInterval(ss)
}

// steadyInterval is the success-path requeue cadence. With a
// driftDetectionInterval set (and genuinely shorter than the effective interval)
// the controller re-asserts the applied state — healing out-of-band drift — on
// that faster cadence, decoupled from the full reconcile interval. A zero/negative
// or not-shorter value is ignored, so it can never become a tight requeue loop.
func (r *StageSetReconciler) steadyInterval(ss *stagesv1.StageSet) time.Duration {
	base := r.effectiveInterval(ss)
	if d := ss.Spec.DriftDetectionInterval; d != nil && d.Duration > 0 && d.Duration < base {
		return d.Duration
	}
	return base
}

func (r *StageSetReconciler) fetcher() *artifact.Fetcher {
	if r.Fetcher != nil {
		return r.Fetcher
	}
	return artifact.New()
}

// failStage records a stage failure on both the Ready condition and the
// per-stage status — including the action ledger (executed) so a retry skips
// the side effects already performed.
//
// Most failures return the cause so controller-runtime backs off and retries.
// Two classes are terminal instead — returning a nil error so the workqueue
// doesn't burn cycles on a failure retry can't fix:
//
//   - a permanent apiserver error during an impersonated apply / connect (RBAC
//     denial, missing CRD, schema rejection) → ReasonRBACDenied;
//   - a terminal fetch error (SSRF rejection, digest mismatch, oversized
//     tarball) → still ReasonStageFailed, but no requeue.
//
// The next genuine watch event (a spec edit, an upstream republish, or the
// interval tick) re-runs the reconcile.
const (
	// maxMigrationFailures is the consecutive-failure count at which a migration
	// transition escalates from retrying (MigrationFailed) to a halted dirty
	// state (MigrationDirty), so the controller stops re-attempting destructive
	// work against an uncertain state.
	maxMigrationFailures = 5
	// opMigration / opMigrationDirty are the failStage op strings that select the
	// migration-specific Ready reasons.
	opMigration      = "migration"
	opMigrationDirty = "migration halted (dirty)"
	// maxActionFailures is the consecutive-failure count at which a stage carrying
	// an incomplete scope: Lifetime action escalates from retrying to a halted
	// ActionDirty state, so a destructive bootstrap stops auto-retrying against an
	// uncertain state.
	maxActionFailures = 5
	// opActionDirty is the failStage op string that selects the ActionDirty Ready
	// reason.
	opActionDirty = "action halted (dirty)"
)

// failReason maps a stage operation and its error to a Ready-condition reason and
// whether the failure is terminal (no controller-runtime backoff). RBAC /
// missing-CRD errors are terminal regardless of op; a terminal fetch error and a
// dirty migration halt are terminal; an ordinary migration failure retries.
func failReason(op string, cause error) (reason string, terminal bool) {
	switch {
	case isPermanentAPIError(cause):
		return ReasonRBACDenied, true
	case op == "fetch artifact" && terminalFetchError(cause):
		return ReasonStageFailed, true
	case op == opMigration:
		return ReasonMigrationFailed, false
	case op == opMigrationDirty:
		return ReasonMigrationDirty, true
	case op == opActionDirty:
		return ReasonActionDirty, true
	default:
		return ReasonStageFailed, false
	}
}

// stageHasPendingLifetime reports whether the stage declares a scope: Lifetime
// action that is not in the already-complete set (done) — an incomplete
// bootstrap. done is the gate's done list for this stage.
func stageHasPendingLifetime(stage *stagesv1.Stage, done []string) bool {
	names := stageLifetimeActionNames(stage)
	if len(names) == 0 {
		return false
	}
	doneSet := make(map[string]bool, len(done))
	for _, d := range done {
		doneSet[d] = true
	}
	for _, n := range names {
		if !doneSet[n] {
			return true
		}
	}
	return false
}

// actionFailureOp selects the failStage op for a pre/post action failure. A
// stage with an incomplete scope: Lifetime action escalates to a halted
// ActionDirty state once failures recur (so a destructive bootstrap stops
// auto-retrying against an uncertain state); every other action failure keeps
// the ordinary retrying op. It increments the persisted failure counter as a
// side effect, mirroring the migration-dirty path.
func (r *StageSetReconciler) actionFailureOp(ss *stagesv1.StageSet, stage *stagesv1.Stage, gate lifetimeGate, phase string) string {
	if !stageHasPendingLifetime(stage, gate.done) {
		return phase
	}
	ss.Status.ActionFailureCount++
	if ss.Status.ActionFailureCount >= maxActionFailures {
		return opActionDirty
	}
	return phase
}

func (r *StageSetReconciler) failStage(ctx context.Context, helper *fluxpatch.Helper, ss *stagesv1.StageSet, stage, op string, cause error, prior []stagesv1.StageStatus, led actionLedger) (ctrl.Result, error) {
	reason, terminal := failReason(op, cause)
	stageMsg := fmt.Sprintf("%s: %v", op, cause)
	readyMsg := fmt.Sprintf("stage %q %s: %v", stage, op, cause)
	switch {
	case reason == ReasonRBACDenied:
		stageMsg = fmt.Sprintf("%s: %s", op, rbacDenialMessage(op, cause))
		readyMsg = fmt.Sprintf("stage %q %s: %s", stage, op, rbacDenialMessage(op, cause))
	case op == "decrypt" || op == "configure decryption":
		// SOPS diagnostics can carry MAC values, key fingerprints, and recipient
		// identifiers. Keep them out of the tenant-readable status condition; the
		// detailed cause is still logged server-side below (and failReason saw it
		// for classification).
		stageMsg = op + ": decryption failed"
		readyMsg = fmt.Sprintf("stage %q %s: decryption failed", stage, op)
	}

	// Merge over the previously persisted entries: prior carries only the stages
	// processed before the failure, and the stages after the failed one were
	// never reached — their persisted records (ledgers, lastHandledPromotion)
	// must survive this write.
	ss.Status.Stages = mergeStageStatuses(ss, append(prior, led.stamp(stagesv1.StageStatus{
		Name:            stage,
		Phase:           stagesv1.StageFailed,
		AppliedRevision: led.revision,
		Message:         stageMsg,
	})))
	publishStageReady(ss)
	ss.Status.ObservedGeneration = ss.Generation
	prevReady := readyConditionSnapshot(ss)
	r.setReady(ss, metav1.ConditionFalse, reason, readyMsg)
	if uerr := r.patchStatus(ctx, helper, ss); uerr != nil {
		return ctrl.Result{}, uerr
	}
	metrics.ReconcileTotal.WithLabelValues(ss.Namespace, ss.Name, reason).Inc()
	r.emitReadyEvent(ss, prevReady, metav1.ConditionFalse, reason, readyMsg)
	log.FromContext(ctx).Error(cause, "stage failed", "stage", stage, "op", op, "terminal", terminal)
	// A terminal failure still has to register as a failure to the stage loop
	// (so the run halts and rollbackOnFailure can engage) WITHOUT engaging
	// controller-runtime's backoff. Wrap the cause in errTerminalStageFailure so
	// `loopErr != nil` still trips; the reconcile's loop-error handler unwraps it
	// to a nil error (no requeue) when returning.
	if terminal {
		return ctrl.Result{}, fmt.Errorf("%w: %w", errTerminalStageFailure, cause)
	}
	return ctrl.Result{}, cause
}

// errTerminalStageFailure marks a stage failure as terminal: the run halts and
// rollbackOnFailure may engage, but controller-runtime must NOT requeue, since
// retry can't fix the cause (an RBAC denial, a digest mismatch, an oversized
// tarball). The reconcile's loop-error handler unwraps it back to a nil error.
var errTerminalStageFailure = errors.New("terminal stage failure")

// runOnFailure runs a stage's onFailure actions best-effort (failures are
// evented, never blocking the failure report). The ledger gates them so a
// repeatedly-failing run fires them only once per pinned revision.
func (r *StageSetReconciler) runOnFailure(ctx context.Context, ss *stagesv1.StageSet, stage *stagesv1.Stage, executor *actions.Executor, done map[string]bool, record func(string) error) {
	if stage.Actions == nil || len(stage.Actions.OnFailure) == 0 {
		return
	}
	if err := executor.Run(ctx, ss.Namespace, stage.Actions.OnFailure, done, record); err != nil {
		r.event(ss, corev1.EventTypeWarning, "OnFailureAction", fmt.Sprintf("stage %q onFailure: %v", stage.Name, err))
	}
}

// runOnRollback runs the StageSet-level spec.onRollback actions best-effort after
// a rollback has restored the previous manifests. It runs under
// spec.serviceAccountName and passes a nil done-set and nil record so the
// per-revision action ledger never suppresses it — the actions fire on every
// rollback (e.g. lifting a maintenance mode a failed upgrade left enabled). A
// failure only emits a Warning event, never blocking the rollback report.
func (r *StageSetReconciler) runOnRollback(ctx context.Context, ss *stagesv1.StageSet, fetcher *artifact.Fetcher, runtimes map[string]*stageRuntime) {
	if len(ss.Spec.OnRollback) == 0 {
		return
	}
	rt, err := r.stageRuntime(ctx, ss, ss.Spec.ServiceAccountName, fetcher, runtimes)
	if err != nil {
		r.event(ss, corev1.EventTypeWarning, "OnRollbackAction", fmt.Sprintf("onRollback: connect to target cluster: %v", err))
		return
	}
	if err := rt.executor.Run(ctx, ss.Namespace, ss.Spec.OnRollback, nil, nil); err != nil {
		r.event(ss, corev1.EventTypeWarning, "OnRollbackAction", fmt.Sprintf("onRollback: %v", err))
	}
}

func indexStageStatuses(stages []stagesv1.StageStatus) map[string]stagesv1.StageStatus {
	m := make(map[string]stagesv1.StageStatus, len(stages))
	for _, s := range stages {
		m[s.Name] = s
	}
	return m
}

// mergeStageStatuses builds the status.stages to persist when a pass ends
// before every spec stage was processed (a promotion or budget hold, a stage
// failure, an unevaluable gate). Entries produced this pass win; a spec stage
// the pass never reached keeps its previously persisted entry, so later
// stages' records — applied revision, action ledger, lastHandledPromotion —
// survive the early exit. Wholesale replacement would erase a later stage's
// lastHandledPromotion, and the promote annotation (which the controller never
// removes) would then replay through that stage's gates as a "fresh" token on
// the next full pass. Entries for stages no longer in the spec drop, matching
// the full-pass write; output is in spec order.
func mergeStageStatuses(ss *stagesv1.StageSet, processed []stagesv1.StageStatus) []stagesv1.StageStatus {
	current := indexStageStatuses(processed)
	prior := indexStageStatuses(ss.Status.Stages)
	merged := make([]stagesv1.StageStatus, 0, len(ss.Spec.Stages))
	for i := range ss.Spec.Stages {
		name := ss.Spec.Stages[i].Name
		if entry, ok := current[name]; ok {
			merged = append(merged, entry)
			continue
		}
		if entry, ok := prior[name]; ok {
			merged = append(merged, entry)
		}
	}
	return merged
}

func toStringSet(items []string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, s := range items {
		m[s] = true
	}
	return m
}

// event emits an events.v1 Event; the reason fills both the reason and action
// slots (we have no separate machine-readable action vocabulary).
// emitVersionSkips fires one ActionSkipped event per Version-scoped action that
// a new revision carried over (rather than re-ran) — the config-churn case the
// scope exists for. It emits only when the revision actually changed, so steady
// reconciles at a fixed revision stay quiet; adoption (no prior revision) does
// not count as a churn skip.
func (r *StageSetReconciler) emitVersionSkips(ss *stagesv1.StageSet, stage *stagesv1.Stage, led actionLedger, priorRevision, revision string) {
	if priorRevision == "" || priorRevision == revision {
		return
	}
	done := led.doneSet()
	for _, name := range versionScopedActionNames(stage) {
		if done[name] {
			r.event(ss, corev1.EventTypeNormal, eventReasonActionSkipped,
				fmt.Sprintf("stage %q action %q (scope: Version) held at version %q despite the new revision; it runs when the version changes", stage.Name, name, led.version))
		}
	}
}

func (r *StageSetReconciler) event(ss *stagesv1.StageSet, eventtype, reason, message string) {
	if r.Recorder != nil {
		r.Recorder.Eventf(ss, nil, eventtype, reason, reason, "%s", message)
	}
}

// eventReasonDriftCorrected is the Event reason for out-of-band drift that the
// apply corrected. It is an Event reason only — the run is still Succeeded
// (the drift was fixed), so it is not a Ready-condition reason.
const eventReasonDriftCorrected = "DriftCorrected"

// eventReasonRolledBack is the Event reason emitted when rollbackOnFailure
// restored the last-good revisions after a failed run.
const eventReasonRolledBack = "RolledBack"

// inventoryReconstructedEvent is the Event reason emitted when a stage's lost
// StageInventory was rebuilt from live cluster objects. It is an Event reason
// only (not a Ready-condition reason): the run still succeeds, and pruning
// resumes on the next reconcile once the inventory is authoritative again.
const inventoryReconstructedEvent = "InventoryReconstructed"

// unionRefs merges two object-reference slices, de-duplicating by object ID.
func unionRefs(a, b []inventory.ObjectRef) []inventory.ObjectRef {
	seen := make(map[string]struct{}, len(a)+len(b))
	out := make([]inventory.ObjectRef, 0, len(a)+len(b))
	for _, refs := range [][]inventory.ObjectRef{a, b} {
		for _, ref := range refs {
			if _, ok := seen[ref.ID()]; ok {
				continue
			}
			seen[ref.ID()] = struct{}{}
			out = append(out, ref)
		}
	}
	return out
}

// reportDrift emits a DriftCorrected Warning Event and bumps a metric when SSA
// changed or recreated an already-managed object on a reconcile that applied
// the SAME revision as last time — i.e. the live object was mutated or deleted
// out-of-band and the apply corrected it. On a new-revision apply, changes are
// the expected rollout (not drift); on the first apply (empty priorRevision)
// there is nothing to compare against.
func (r *StageSetReconciler) reportDrift(ss *stagesv1.StageSet, stage *stagesv1.Stage, cs *ssa.ChangeSet, priorRevision, currentRevision string) {
	if cs == nil || priorRevision == "" || priorRevision != currentRevision {
		return
	}
	var drifted []string
	for _, e := range cs.Entries {
		if e.Action == ssa.CreatedAction || e.Action == ssa.ConfiguredAction {
			drifted = append(drifted, e.Subject)
		}
	}
	if len(drifted) == 0 {
		return
	}
	metrics.DriftCorrectedTotal.WithLabelValues(ss.Namespace, ss.Name, stage.Name).Add(float64(len(drifted)))
	r.event(ss, corev1.EventTypeWarning, eventReasonDriftCorrected,
		fmt.Sprintf("stage %q corrected out-of-band drift on %d object(s): %s",
			stage.Name, len(drifted), strings.Join(drifted, ", ")))
}

// manifestApplier adapts the apply engine to the actions.ManifestApplier seam,
// so an apply action can SSA-apply transient manifests (and optionally wait for
// readiness) without the actions package depending on internal/apply. The
// objects get owner labels like any applied object but are never recorded in a
// StageInventory, so the inventory diff never prunes them.
type manifestApplier struct {
	applier         *apply.Applier
	name, namespace string
}

func (m *manifestApplier) Apply(ctx context.Context, objects []*unstructured.Unstructured, wait bool, timeout time.Duration) error {
	cs, err := m.applier.Apply(ctx, m.name, m.namespace, objects, apply.ConflictHandling{})
	if err != nil {
		return err
	}
	if wait {
		return m.applier.Wait(ctx, cs.ToObjMetadataSet(), timeout)
	}
	return nil
}

// stageRuntime bundles the target-cluster connection and the apply/action
// engines a stage's cluster operations run through. Every field derives from the
// stage's effective ServiceAccount, so all of a stage's writes — apply, prune,
// readiness verification, actions — share one identity, and two stages with the
// same effective SA share the same cached connection and minted token.
type stageRuntime struct {
	target   client.Client
	mapper   apimeta.RESTMapper
	applier  *apply.Applier
	executor *actions.Executor
}

// effectiveServiceAccount is the ServiceAccount a stage's cluster operations run
// under: the stage's own serviceAccountName when set, otherwise the StageSet's
// spec.serviceAccountName. Per-stage overrides let one StageSet drive stages that
// target different tenants, each bounded by its own ServiceAccount's RBAC.
func effectiveServiceAccount(ss *stagesv1.StageSet, stage *stagesv1.Stage) string {
	if stage.ServiceAccountName != "" {
		return stage.ServiceAccountName
	}
	return ss.Spec.ServiceAccountName
}

// stageRuntime resolves (and memoizes in cache) the runtime for an effective SA.
// The key is the SA string alone: spec.kubeConfig is StageSet-level, so it is the
// same for every stage, and targetCluster already caches the underlying client
// and token per (namespace, SA). Stages sharing an SA therefore reuse one
// connection while distinct SAs stay RBAC-isolated.
func (r *StageSetReconciler) stageRuntime(ctx context.Context, ss *stagesv1.StageSet, sa string, fetcher *artifact.Fetcher, cache map[string]*stageRuntime) (*stageRuntime, error) {
	if rt, ok := cache[sa]; ok {
		return rt, nil
	}
	target, mapper, err := r.targetCluster(ctx, ss.Namespace, sa, ss.Spec.KubeConfig)
	if err != nil {
		return nil, err
	}
	applier := apply.New(target, mapper, stagesv1.GroupVersion.Group)
	rt := &stageRuntime{
		target:  target,
		mapper:  mapper,
		applier: applier,
		executor: &actions.Executor{
			Client:       target,
			AllowedHosts: r.AllowedActionHosts,
			IPValidator:  r.ActionIPValidator,
			Resolver:     &artifact.Resolver{NoCrossNamespace: r.NoCrossNamespaceRefs},
			Fetcher:      fetcher,
			Applier:      &manifestApplier{applier: applier, name: ss.Name, namespace: ss.Namespace},
		},
	}
	cache[sa] = rt
	return rt, nil
}

func disableWait(stage *stagesv1.Stage) bool {
	return stage.ReadyChecks != nil && stage.ReadyChecks.DisableWait
}

// stagePrune reports whether a stage garbage-collects objects that fell out of
// its inventory (default true).
func stagePrune(stage *stagesv1.Stage) bool {
	return stage.Prune == nil || *stage.Prune
}

// reconcileDelete tears the StageSet's applied objects down in reverse stage
// order (skipping prune:false stages, whose objects are deliberately
// orphaned), then drops the finalizer so the apiserver can complete deletion —
// the owned StageInventory shards are GC'd by their owner reference.
//
// A teardown failure normally returns the error so the finalizer stays and
// controller-runtime retries. But an unreachable target (deleted kubeConfig
// Secret, revoked RBAC, decommissioned cluster) would otherwise wedge the
// StageSet in Terminating forever. teardownTimedOut caps that wait: once the
// deletion has been pending longer than --max-teardown-wait, the finalizer is
// force-dropped (emitting a Warning TeardownForced event and a metric, leaving
// whatever objects couldn't be deleted orphaned for an operator to clean up).
func (r *StageSetReconciler) reconcileDelete(ctx context.Context, ss *stagesv1.StageSet) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(ss, FinalizerName) {
		return ctrl.Result{}, nil
	}
	// Teardown deletes each stage's objects under the same identity that applied
	// them: its effective ServiceAccount. A recorded stage still present in the
	// spec resolves that SA (stage override, else the StageSet default); a stage
	// already removed from the spec no longer carries one, so it falls back to the
	// StageSet default. The default runtime is resolved up front — it validates
	// spec.kubeConfig and backs the removed-stage fallback.
	fetcher := r.fetcher()
	runtimes := map[string]*stageRuntime{}
	defaultRT, err := r.stageRuntime(ctx, ss, ss.Spec.ServiceAccountName, fetcher, runtimes)
	if err != nil {
		return r.teardownFailure(ctx, ss, "connect to target cluster", err)
	}
	recorder := &stageinv.Recorder{Client: r.Client, ShardCap: r.ShardCap}
	records, err := recorder.StageRecords(ctx, ss.Name, ss.Namespace)
	if err != nil {
		return r.teardownFailure(ctx, ss, "read stage inventory", err)
	}
	specStages := make(map[string]*stagesv1.Stage, len(ss.Spec.Stages))
	for i := range ss.Spec.Stages {
		specStages[ss.Spec.Stages[i].Name] = &ss.Spec.Stages[i]
	}
	prune := stagePruneByName(ss)
	for _, stage := range stagesByPositionDesc(records) {
		if allowed, known := prune[stage]; known && !allowed {
			continue // prune:false: objects orphaned deliberately
		}
		refs := records[stage].Refs
		if len(refs) == 0 {
			continue
		}
		delApplier := defaultRT.applier
		if st, ok := specStages[stage]; ok {
			srt, rerr := r.stageRuntime(ctx, ss, effectiveServiceAccount(ss, st), fetcher, runtimes)
			if rerr != nil {
				return r.teardownFailure(ctx, ss, fmt.Sprintf("connect for stage %q teardown", stage), rerr)
			}
			delApplier = srt.applier
		}
		if _, derr := delApplier.Delete(ctx, ss.Name, ss.Namespace, stageinv.Objects(refs)); derr != nil {
			return r.teardownFailure(ctx, ss, fmt.Sprintf("delete stage %q objects", stage), derr)
		}
	}
	metrics.DeleteStageReady(ss.Namespace, ss.Name)
	metrics.DeleteStageSetMetrics(ss.Namespace, ss.Name)
	controllerutil.RemoveFinalizer(ss, FinalizerName)
	return ctrl.Result{}, r.Update(ctx, ss)
}

// teardownFailure handles a failed step of reverse-order teardown. While the
// deletion has been pending less than --max-teardown-wait it returns the error
// so the finalizer stays and controller-runtime retries. Past the bound it
// force-drops the finalizer (Warning event + metric) so a permanently-broken
// target can't pin the StageSet in Terminating indefinitely — at the cost of
// orphaning whatever objects could not be deleted.
func (r *StageSetReconciler) teardownFailure(ctx context.Context, ss *stagesv1.StageSet, op string, cause error) (ctrl.Result, error) {
	timedOut, elapsed := r.teardownTimedOut(ss)
	if !timedOut {
		return ctrl.Result{}, cause // retry; finalizer stays
	}
	msg := fmt.Sprintf("TeardownForced after %s of failing teardown (%s) — finalizer dropped; the target cluster may carry orphaned objects an operator must remove by hand. Last error: %v",
		elapsed.Round(time.Second), op, cause)
	metrics.DeleteStageReady(ss.Namespace, ss.Name)
	metrics.DeleteStageSetMetrics(ss.Namespace, ss.Name)
	controllerutil.RemoveFinalizer(ss, FinalizerName)
	if err := r.Update(ctx, ss); err != nil {
		// The finalizer is still on. Emit nothing here: the retry re-decides and
		// emits once the Update lands, so the TeardownForced event + the
		// stageset_teardown_force_drop_total metric fire once per actual drop,
		// not once per failed-Update retry (which would inflate the alert metric).
		return ctrl.Result{}, err
	}
	log.FromContext(ctx).Error(cause, "force-dropped finalizer after --max-teardown-wait",
		"elapsed", elapsed.String(), "op", op)
	metrics.TeardownForceDropTotal.WithLabelValues(ss.Namespace, ss.Name).Inc()
	r.event(ss, corev1.EventTypeWarning, "TeardownForced", msg)
	return ctrl.Result{}, nil
}

// teardownTimedOut reports whether the StageSet has been in the deletion path
// longer than the effective --max-teardown-wait, returning the elapsed time for
// the Warning event + log line. A zero DeletionTimestamp (impossible in
// practice — reconcileDelete runs only after the timestamp lands) means "not
// timed out".
func (r *StageSetReconciler) teardownTimedOut(ss *stagesv1.StageSet) (bool, time.Duration) {
	if ss.DeletionTimestamp.IsZero() {
		return false, 0
	}
	wait := r.MaxTeardownWait
	if wait <= 0 {
		wait = defaultMaxTeardownWait
	}
	elapsed := r.now().Sub(ss.DeletionTimestamp.Time)
	return elapsed >= wait, elapsed
}

// publishStageReady mirrors the StageSet's per-stage phases into the
// stageset_stage_ready gauge so metric-based progressive-delivery (e.g. Argo
// Rollouts) can gate on a stage without calling the HTTP gate.
func publishStageReady(ss *stagesv1.StageSet) {
	for _, s := range ss.Status.Stages {
		metrics.SetStageReady(ss.Namespace, ss.Name, s.Name, s.Phase == stagesv1.StageReady)
	}
}

// toInventoryRecords converts stored stage records into the inventory
// package's record type for ownership-transfer planning.
func toInventoryRecords(m map[string]stageinv.StageRecord) []inventory.StageRecord {
	out := make([]inventory.StageRecord, 0, len(m))
	for name, rec := range m {
		out = append(out, inventory.StageRecord{Name: name, Position: rec.Position, Entries: rec.Refs})
	}
	return out
}

// stagesByPositionDesc orders stage names by recorded position descending
// (later stages first), with name as a stable tie-break.
func stagesByPositionDesc(records map[string]stageinv.StageRecord) []string {
	names := make([]string, 0, len(records))
	for name := range records {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		if pi, pj := records[names[i]].Position, records[names[j]].Position; pi != pj {
			return pi > pj
		}
		return names[i] < names[j]
	})
	return names
}

// stagePruneByName maps each spec stage to whether it prunes (default true).
func stagePruneByName(ss *stagesv1.StageSet) map[string]bool {
	m := make(map[string]bool, len(ss.Spec.Stages))
	for i := range ss.Spec.Stages {
		m[ss.Spec.Stages[i].Name] = stagePrune(&ss.Spec.Stages[i])
	}
	return m
}

// cycleSentinel is the dependenciesReady "why" value signalling a dependsOn
// cycle (kept out of the normal message space).
const cycleSentinel = "\x00cycle"

// dependenciesReady reports whether every spec.dependsOn StageSet is Ready at
// its observed generation. A non-empty why explains a not-ready result;
// cycleSentinel signals a terminal dependsOn cycle.
func (r *StageSetReconciler) dependenciesReady(ctx context.Context, ss *stagesv1.StageSet) (bool, string, error) {
	if len(ss.Spec.DependsOn) == 0 {
		return true, "", nil
	}
	cyclic, err := r.hasDependencyCycle(ctx, ss)
	if err != nil {
		return false, "", err
	}
	if cyclic {
		return false, cycleSentinel, nil
	}
	for _, dep := range ss.Spec.DependsOn {
		ns := dep.Namespace
		if ns == "" {
			ns = ss.Namespace
		}
		if ns != ss.Namespace && r.NoCrossNamespaceRefs {
			return false, fmt.Sprintf("cross-namespace dependsOn %s/%s rejected", ns, dep.Name), nil
		}
		var d stagesv1.StageSet
		if gerr := r.graphReader().Get(ctx, types.NamespacedName{Namespace: ns, Name: dep.Name}, &d); gerr != nil {
			if apierrors.IsNotFound(gerr) {
				return false, fmt.Sprintf("dependency %s/%s not found", ns, dep.Name), nil
			}
			return false, "", gerr
		}
		if !isReady(&d) || d.Status.ObservedGeneration != d.Generation {
			return false, fmt.Sprintf("dependency %s/%s is not ready", ns, dep.Name), nil
		}
		// A dependency that is Deployed-and-Ready but holding a NEW revision behind
		// an update window still reports Ready=True with ObservedGeneration current
		// (its already-deployed state is healthy). Rolling a dependent out now would
		// build it against the dependency's old, about-to-be-replaced revision, so
		// treat a pending not-yet-applied revision as not-ready for dependents.
		// PendingUpdate.Revisions is populated only when a new revision is held — a
		// pure windowScope=All freeze with no new revision leaves it empty and does
		// not block dependents.
		if d.Status.PendingUpdate != nil && len(d.Status.PendingUpdate.Revisions) > 0 {
			return false, fmt.Sprintf("dependency %s/%s has a new revision held by an update window", ns, dep.Name), nil
		}
	}
	return true, "", nil
}

// graphReader is the reader the cross-object dependency/source walk uses: the
// uncached APIReader when wired, else the embedded (possibly cache-scoped)
// client. The uncached reader sees dependencies in namespaces the manager's
// cache does not scope, so an out-of-scope dependsOn/sourceRef resolves (or is a
// genuine NotFound leaf) instead of erroring with "unknown namespace for the
// cache" and wedging the reconcile in transient backoff.
func (r *StageSetReconciler) graphReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// hasDependencyCycle walks the dependsOn graph breadth-first and reports
// whether a path leads back to the starting StageSet.
func (r *StageSetReconciler) hasDependencyCycle(ctx context.Context, ss *stagesv1.StageSet) (bool, error) {
	start := ss.Namespace + "/" + ss.Name
	seen := map[string]bool{}
	queue := dependsOnKeys(ss)
	for len(queue) > 0 {
		k := queue[0]
		queue = queue[1:]
		if k == start {
			return true, nil
		}
		if seen[k] {
			continue
		}
		seen[k] = true
		ns, name, ok := splitKey(k)
		if !ok {
			continue
		}
		var d stagesv1.StageSet
		if err := r.graphReader().Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &d); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return false, err
		}
		queue = append(queue, dependsOnKeys(&d)...)
	}
	return false, nil
}

func dependsOnKeys(ss *stagesv1.StageSet) []string {
	keys := make([]string, 0, len(ss.Spec.DependsOn))
	for _, dep := range ss.Spec.DependsOn {
		ns := dep.Namespace
		if ns == "" {
			ns = ss.Namespace
		}
		keys = append(keys, ns+"/"+dep.Name)
	}
	return keys
}

func splitKey(k string) (ns, name string, ok bool) {
	before, after, ok := strings.Cut(k, "/")
	if !ok {
		return "", "", false
	}
	return before, after, true
}

func isReady(ss *stagesv1.StageSet) bool {
	c := apimeta.FindStatusCondition(ss.Status.Conditions, ConditionReady)
	return c != nil && c.Status == metav1.ConditionTrue
}

func stageTimeout(ss *stagesv1.StageSet, stage *stagesv1.Stage) time.Duration {
	// A non-positive timeout is treated as unset, not "expire immediately": a
	// zero duration would make the verify wait's context expire at once and fail
	// every wait-enabled stage instantly. Fall through to the next level so an
	// explicit 0s means "use the default" as operators expect.
	if stage.Timeout != nil && stage.Timeout.Duration > 0 {
		return stage.Timeout.Duration
	}
	if ss.Spec.Timeout != nil && ss.Spec.Timeout.Duration > 0 {
		return ss.Spec.Timeout.Duration
	}
	return 5 * time.Minute
}

// resolvePostBuildVars assembles the substitution map from substituteFrom
// (ConfigMaps/Secrets in the StageSet's namespace) overlaid with inline
// substitute values, which take precedence.
//
// The referenced objects are read under the StageSet's serviceAccountName, so a
// tenant can only substitute from data its ServiceAccount can read — the same
// rule buildDecryptor applies to key material. Reading them as the controller
// would let anyone who can write a StageSet fold any Secret in the namespace
// into the rendered manifests, which are then applied as objects they can read
// back, regardless of their own RBAC.
//
// An empty serviceAccountName falls back to the controller's client inside
// targetCluster; that is the single-tenant path, where there is no tenant
// identity to bound the read to.
func (r *StageSetReconciler) resolvePostBuildVars(ctx context.Context, ss *stagesv1.StageSet, pb *stagesv1.PostBuild) (map[string]string, error) {
	if pb == nil {
		return nil, nil
	}
	vars := map[string]string{}
	// Only substituteFrom reaches the API. A spec carrying just inline
	// substitute values resolves without a client at all, so it neither mints a
	// token nor fails when the ServiceAccount is missing.
	if len(pb.SubstituteFrom) > 0 {
		tenant, _, err := r.targetCluster(ctx, ss.Namespace, ss.Spec.ServiceAccountName, nil)
		if err != nil {
			return nil, fmt.Errorf("substituteFrom: %w", err)
		}
		for _, ref := range pb.SubstituteFrom {
			key := types.NamespacedName{Namespace: ss.Namespace, Name: ref.Name}
			switch ref.Kind {
			case "ConfigMap":
				var cm corev1.ConfigMap
				if err := tenant.Get(ctx, key, &cm); err != nil {
					if ref.Optional && apierrors.IsNotFound(err) {
						continue
					}
					return nil, fmt.Errorf("substituteFrom ConfigMap %q: %w", ref.Name, err)
				}
				maps.Copy(vars, cm.Data)
			case "Secret":
				var sec corev1.Secret
				if err := tenant.Get(ctx, key, &sec); err != nil {
					if ref.Optional && apierrors.IsNotFound(err) {
						continue
					}
					return nil, fmt.Errorf("substituteFrom Secret %q: %w", ref.Name, err)
				}
				for k, v := range sec.Data {
					vars[k] = string(v)
				}
			}
		}
	}
	maps.Copy(vars, pb.Substitute)
	return vars, nil
}

// SetupWithManager wires the watches: the StageSet itself, owned
// StageInventory shards, StageSet dependents (dependsOn wake-ups), and — when
// the ExternalArtifact kind is installed — ExternalArtifact changes mapped back
// to the StageSets that reference them, so a new artifact revision triggers an
// immediate reconcile instead of waiting for the interval.
func (r *StageSetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.RESTMapper == nil {
		r.RESTMapper = mgr.GetRESTMapper()
	}
	if r.Config == nil {
		r.Config = mgr.GetConfig()
	}
	if r.APIReader == nil {
		r.APIReader = mgr.GetAPIReader()
	}
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorder("stageset-controller")
	}
	// Default the token minter (and its cache) for the local-cluster
	// identity-assumption path, unless impersonation is skipped (envtest) or
	// the test already wired a fake minter.
	if !r.SkipImpersonation && r.minter == nil && r.Config != nil {
		kc, err := kubernetes.NewForConfig(r.Config)
		if err != nil {
			return fmt.Errorf("build clientset for token minting: %w", err)
		}
		r.minter = clientsetTokenMinter{kc: kc}
	}
	if r.minter != nil && r.tokens == nil {
		r.tokens = newTokenCache(r.minter)
	}
	if r.remoteConfig == nil {
		r.remoteConfig = defaultRemoteConfigBuilder{r: r}
	}
	// Default the metric-source querier for the error-budget freeze and
	// promotion-analysis gates. A metric query is an outbound call to a URL the
	// StageSet author chose, so it carries the same two guards an http action
	// does: its dialer pins every resolved address through MetricIPValidator
	// (production denylist when nil), and AllowedActionHosts bounds the hosts it
	// may reach. Optional bearer-token Secrets are read as the StageSet's
	// ServiceAccount — see readSecretData.
	if r.MetricQuerier == nil {
		r.MetricQuerier = metricsource.New(r.readSecretData, r.MetricIPValidator, r.AllowedActionHosts)
	}

	// Spread interval-based requeues by +/- defaultIntervalJitterFraction so a
	// fleet of StageSets sharing a --default-interval doesn't thunder-herd the
	// controller (and the upstream producers) on one deadline. Setting the
	// global jitter is idempotent for the same fraction, so repeated
	// SetupWithManager calls (multiple test cases in one binary) are safe. A
	// nil rand selects a time-seeded one.
	jitter.SetGlobalIntervalJitter(defaultIntervalJitterFraction, nil)

	b := ctrl.NewControllerManagedBy(mgr).
		For(&stagesv1.StageSet{}, crbuilder.WithPredicates(
			// Wake on a spec change (generation bump), a fresh
			// reconcile.fluxcd.io/requestedAt token (whole-object force
			// reconcile), or a stages.metio.wtf/reconcile-stage change
			// (single-stage force reconcile). Filtering out the status-only
			// updates the reconciler writes itself keeps the workqueue from
			// churning on its own condition/observedGeneration stamps;
			// spec.interval (jittered RequeueAfter) drives the steady-state
			// reconcile, and the StageInventory / dependsOn / ExternalArtifact
			// watches drive dependency-triggered runs.
			predicate.Or(
				predicate.GenerationChangedPredicate{},
				fluxpredicates.ReconcileRequestedPredicate{},
				reconcileStageRequestedPredicate{},
				migrationApprovalPredicate{},
				promoteRequestedPredicate{},
			),
		)).
		Owns(&stagesv1.StageInventory{}).
		Watches(&stagesv1.StageSet{}, handler.EnqueueRequestsFromMapFunc(r.mapStageSetDependents)).
		// A StageLedger is not owned (retain-always), so Owns won't wake its
		// StageSet. Watch it by name so a spec.baseline applied after the
		// StageSet last reconciled is promoted without waiting out the interval.
		Watches(&stagesv1.StageLedger{}, handler.EnqueueRequestsFromMapFunc(mapSameName))

	// Gate the ExternalArtifact watch on the kind being installed so the
	// controller boots cleanly in clusters without source-controller.
	eaGVK := artifact.ExternalArtifactGVK
	if _, err := mgr.GetRESTMapper().RESTMapping(eaGVK.GroupKind(), eaGVK.Version); err == nil {
		ea := &unstructured.Unstructured{}
		ea.SetGroupVersionKind(eaGVK)
		b = b.Watches(ea, handler.EnqueueRequestsFromMapFunc(r.mapExternalArtifact))
	}
	// Build (not Complete) so producer watches can be added at runtime: a
	// sourceRef can name any producer kind, unknown until reconcile.
	c, err := b.Build(r)
	if err != nil {
		return err
	}
	r.controller = c
	r.mgrCache = mgr.GetCache()
	r.watchedProducers = map[schema.GroupVersionKind]struct{}{}
	return nil
}

// mapExternalArtifact maps an ExternalArtifact change to the StageSets (in the
// same namespace) whose stages reference it — directly or through the RFC-0012
// producer back-pointer.
func (r *StageSetReconciler) mapExternalArtifact(ctx context.Context, obj client.Object) []reconcile.Request {
	ea, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return nil
	}
	var list stagesv1.StageSetList
	if err := r.List(ctx, &list, client.InNamespace(ea.GetNamespace())); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for i := range list.Items {
		ss := &list.Items[i]
		for j := range ss.Spec.Stages {
			if sourceRefMatchesEA(ss.Spec.Stages[j].SourceRef, ea) {
				reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ss.Namespace, Name: ss.Name}})
				break
			}
		}
	}
	return reqs
}

// mapStageSetDependents maps a StageSet change to the StageSets that dependOn
// it, so a dependency becoming Ready wakes its dependents immediately.
// mapSameName enqueues the StageSet sharing an object's namespace and name — the
// StageLedger↔StageSet identity is (namespace, name).
func mapSameName(_ context.Context, obj client.Object) []reconcile.Request {
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: obj.GetName()}}}
}

func (r *StageSetReconciler) mapStageSetDependents(ctx context.Context, obj client.Object) []reconcile.Request {
	dep, ok := obj.(*stagesv1.StageSet)
	if !ok {
		return nil
	}
	var list stagesv1.StageSetList
	if err := r.List(ctx, &list, client.InNamespace(dep.Namespace)); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for i := range list.Items {
		ss := &list.Items[i]
		for _, d := range ss.Spec.DependsOn {
			dns := d.Namespace
			if dns == "" {
				dns = ss.Namespace
			}
			if d.Name == dep.Name && dns == dep.Namespace {
				reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ss.Namespace, Name: ss.Name}})
				break
			}
		}
	}
	return reqs
}

// sourceRefMatchesEA reports whether a stage sourceRef resolves to the given
// ExternalArtifact (directly by name, or via its producer back-pointer).
func sourceRefMatchesEA(ref stagesv1.SourceReference, ea *unstructured.Unstructured) bool {
	kind := ref.Kind
	if kind == "" {
		kind = "ExternalArtifact"
	}
	if kind == "ExternalArtifact" {
		return ref.Name == ea.GetName()
	}
	bp, found, err := unstructured.NestedStringMap(ea.Object, "spec", "sourceRef")
	if err != nil || !found {
		return false
	}
	return bp["kind"] == ref.Kind && bp["name"] == ref.Name && groupOf(bp["apiVersion"]) == groupOf(ref.APIVersion)
}

// producerGVK derives the GVK of a producer sourceRef. APIVersion defaults to
// the Flux source group, matching the resolver. A nil GVK (unparseable) is
// ignored by the watch engagement.
func producerGVK(ref stagesv1.SourceReference) schema.GroupVersionKind {
	apiVersion := ref.APIVersion
	if apiVersion == "" {
		apiVersion = artifact.ExternalArtifactGVK.GroupVersion().String()
	}
	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return schema.GroupVersionKind{}
	}
	return gv.WithKind(ref.Kind)
}

// isProducerRef reports whether a sourceRef names a producer (anything other
// than a direct ExternalArtifact), which is what gets a dynamic watch.
func isProducerRef(ref stagesv1.SourceReference) bool {
	return ref.Kind != "" && ref.Kind != artifact.ExternalArtifactGVK.Kind
}

// engageProducerWatch adds a dynamic watch on a producer kind the first time a
// StageSet references it, so the producer FAILING (a status change that
// publishes no new artifact) surfaces on the referencing StageSet immediately
// instead of waiting for retryInterval. ExternalArtifact is already watched
// statically; an uninstalled producer kind is skipped and retried on a later
// reconcile once its CRD exists. Idempotent and concurrency-safe; only a
// successful Watch records the GVK, so a transient failure re-engages.
func (r *StageSetReconciler) engageProducerWatch(gvk schema.GroupVersionKind) {
	if r.controller == nil || gvk.Empty() || gvk == artifact.ExternalArtifactGVK {
		return
	}
	r.watchMu.Lock()
	defer r.watchMu.Unlock()
	if _, ok := r.watchedProducers[gvk]; ok {
		return
	}
	if _, err := r.RESTMapper.RESTMapping(gvk.GroupKind(), gvk.Version); err != nil {
		return // kind not installed yet; engage on a later reconcile
	}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	src := source.Kind(r.mgrCache, client.Object(obj), handler.EnqueueRequestsFromMapFunc(r.mapProducer))
	if err := r.controller.Watch(src); err != nil {
		// A failed engagement is otherwise silent: the producer kind stays
		// unwatched, so dependent StageSets stop re-triggering on its upstream
		// changes until a later reconcile re-attempts. Count it so a sustained
		// pattern surfaces in Prometheus even though the next reconcile retries.
		metrics.WatchEngagementFailuresTotal.WithLabelValues(gvk.String()).Inc()
		return
	}
	r.watchedProducers[gvk] = struct{}{}
}

// mapProducer maps a producer object's change to the StageSets whose sourceRef
// names it (same namespace, mirroring mapExternalArtifact), so a failing
// producer surfaces on its consumers without waiting for retryInterval.
func (r *StageSetReconciler) mapProducer(ctx context.Context, obj client.Object) []reconcile.Request {
	gvk := obj.GetObjectKind().GroupVersionKind()
	var list stagesv1.StageSetList
	if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for i := range list.Items {
		ss := &list.Items[i]
		for j := range ss.Spec.Stages {
			ref := ss.Spec.Stages[j].SourceRef
			if isProducerRef(ref) && ref.Name == obj.GetName() && producerGVK(ref) == gvk {
				reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ss.Namespace, Name: ss.Name}})
				break
			}
		}
	}
	return reqs
}

func groupOf(apiVersion string) string {
	if before, _, ok := strings.Cut(apiVersion, "/"); ok {
		return before
	}
	return ""
}
