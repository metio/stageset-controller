// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package v1

import (
	"github.com/fluxcd/pkg/apis/kustomize"
	"github.com/fluxcd/pkg/apis/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// StageSetSpec defines an ordered, gated, multi-stage deployment of
// ExternalArtifact contents.
type StageSetSpec struct {
	// Interval at which to reconcile the StageSet. Optional: when omitted, the
	// controller's --default-interval is used.
	// +optional
	Interval metav1.Duration `json:"interval,omitempty"`

	// RetryInterval overrides Interval after a failed run.
	// +optional
	RetryInterval *metav1.Duration `json:"retryInterval,omitempty"`

	// DriftDetectionInterval, when set, re-asserts the applied state on this
	// (typically shorter) cadence to correct out-of-band drift, independently of
	// the full reconcile Interval. Source changes still reconcile immediately via
	// watches; this only controls how often the controller re-applies the pinned
	// state to heal drift. Leave unset to fold drift correction into Interval.
	// +optional
	DriftDetectionInterval *metav1.Duration `json:"driftDetectionInterval,omitempty"`

	// Timeout is the default per-stage timeout, overridable per stage.
	// +optional
	Timeout *metav1.Duration `json:"timeout,omitempty"`

	// Suspend pauses reconciliation.
	// +optional
	Suspend bool `json:"suspend,omitempty"`

	// DependsOn lists StageSets that must be Ready (with observed
	// generation) before this one reconciles. Semantics match
	// kustomize-controller.
	// +optional
	DependsOn []meta.NamespacedObjectReference `json:"dependsOn,omitempty"`

	// ServiceAccountName impersonated for all cluster operations,
	// including actions.
	// +optional
	ServiceAccountName string `json:"serviceAccountName,omitempty"`

	// KubeConfig selects a remote cluster to apply to.
	// +optional
	KubeConfig *meta.KubeConfigReference `json:"kubeConfig,omitempty"`

	// Decryption decrypts SOPS-encrypted files in every stage's source before
	// the manifests are built, so an encrypted Secret rolls out as plaintext.
	// +optional
	Decryption *Decryption `json:"decryption,omitempty"`

	// Version identifies the deployed system's version so versioned
	// migrations can gate on transitions. Unset disables versioning and
	// migrations entirely.
	// +optional
	Version *VersionSource `json:"version,omitempty"`

	// Migrations are version-gated action ladders run when crossing a
	// version boundary, anchored before a named stage's pre-actions.
	// Requires spec.version.
	// +optional
	Migrations []Migration `json:"migrations,omitempty"`

	// RollbackOnFailure restores the last-applied artifact revisions under
	// the current spec when a run fails (best-effort: works only while the
	// producer retains the previous revision).
	// +optional
	RollbackOnFailure bool `json:"rollbackOnFailure,omitempty"`

	// UpdateWindows gate when new revisions may roll out. Empty means always.
	// Deny windows take precedence; if any Allow window is declared, updates
	// happen only while an Allow is active and no Deny is.
	// +optional
	UpdateWindows []UpdateWindow `json:"updateWindows,omitempty"`

	// WindowScope chooses what a closed update window blocks: Updates
	// (default — hold only new-revision rollouts, keep correcting drift) or
	// All (hard freeze — also pause drift correction).
	// +kubebuilder:validation:Enum=Updates;All
	// +optional
	WindowScope string `json:"windowScope,omitempty"`

	// Stages is the ordered list of stages.
	// +kubebuilder:validation:MinItems=1
	// +required
	Stages []Stage `json:"stages"`
}

// UpdateWindow is one allow/deny window. It is either recurring (schedule +
// duration) or absolute (from + to), never both.
type UpdateWindow struct {
	// Type is Allow or Deny. Deny takes precedence over Allow.
	// +kubebuilder:validation:Enum=Allow;Deny
	// +required
	Type string `json:"type"`

	// Schedule is a standard 5-field cron expression for a recurring window's
	// start. Requires duration; mutually exclusive with from/to.
	// +optional
	Schedule string `json:"schedule,omitempty"`

	// Duration of a recurring window.
	// +optional
	Duration *metav1.Duration `json:"duration,omitempty"`

	// TimeZone is the IANA name the schedule is evaluated in. Defaults to UTC.
	// +optional
	TimeZone string `json:"timeZone,omitempty"`

	// From is the start of an absolute one-off window; mutually exclusive with
	// schedule.
	// +optional
	From *metav1.Time `json:"from,omitempty"`

	// To is the exclusive end of an absolute window.
	// +optional
	To *metav1.Time `json:"to,omitempty"`
}

// Stage is one ordered, gated unit of a StageSet.
type Stage struct {
	// Name of the stage; unique within the StageSet, used as inventory key.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +required
	Name string `json:"name"`

	// SourceRef references the ExternalArtifact providing this stage's
	// manifests.
	// +required
	SourceRef SourceReference `json:"sourceRef"`

	// Path inside the artifact to build. Defaults to "./".
	// +optional
	Path string `json:"path,omitempty"`

	// Prune enables garbage collection of objects that fell out of this
	// stage. Defaults to true.
	// +optional
	Prune *bool `json:"prune,omitempty"`

	// Timeout overrides the StageSet-level default for this stage.
	// +optional
	Timeout *metav1.Duration `json:"timeout,omitempty"`

	// Force recreates objects on immutable-field conflicts.
	// +optional
	Force bool `json:"force,omitempty"`

	// ConflictPolicy gives per-resource answers to immutable-field
	// conflicts (Fail/Recreate/KeepExisting), superseding the blunt Force
	// toggle (which is sugar for default: Recreate).
	// +optional
	ConflictPolicy *ConflictPolicy `json:"conflictPolicy,omitempty"`

	// ApplyHelmHookResources controls handling of resources carrying
	// helm.sh/hook annotations: applied as ordinary resources with a
	// warning Event (true, the default) or stripped at build time (false).
	// +optional
	ApplyHelmHookResources *bool `json:"applyHelmHookResources,omitempty"`

	// Patches are Kustomize patches applied post-build.
	// +optional
	Patches []kustomize.Patch `json:"patches,omitempty"`

	// PostBuild runs variable substitution after build and patching.
	// +optional
	PostBuild *PostBuild `json:"postBuild,omitempty"`

	// Actions are typed pre/post/onFailure steps around the stage.
	// +optional
	Actions *StageActions `json:"actions,omitempty"`

	// ReadyChecks gate stage completion; purely observational.
	// +optional
	ReadyChecks *ReadyChecks `json:"readyChecks,omitempty"`
}

// SourceReference names either an ExternalArtifact directly (the default
// when Kind and APIVersion are omitted) or the producer object behind one.
// Producer references are resolved through the ExternalArtifact's RFC-0012
// spec.sourceRef back-pointer; resolution always lands on an
// ExternalArtifact, keeping the data plane single-kind.
type SourceReference struct {
	// APIVersion of the referent. Defaults to
	// source.toolkit.fluxcd.io/v1. Required when Kind is set to a
	// producer kind.
	// +optional
	APIVersion string `json:"apiVersion,omitempty"`

	// Kind of the referent. Defaults to ExternalArtifact; any other kind
	// is treated as a producer and resolved via the back-pointer index.
	// +optional
	Kind string `json:"kind,omitempty"`

	// Name of the referent.
	// +required
	Name string `json:"name"`

	// Namespace of the referent; defaults to the StageSet's namespace.
	// Cross-namespace references are gated by --no-cross-namespace-refs.
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// PostBuild mirrors kustomize-controller's post-build substitution.
type PostBuild struct {
	// Substitute holds inline key/value pairs.
	// +optional
	Substitute map[string]string `json:"substitute,omitempty"`

	// SubstituteFrom pulls key/value pairs from ConfigMaps or Secrets.
	// +optional
	SubstituteFrom []SubstituteReference `json:"substituteFrom,omitempty"`
}

// SubstituteReference selects a ConfigMap or Secret for substitution input.
type SubstituteReference struct {
	// Kind of the referent: ConfigMap or Secret.
	// +kubebuilder:validation:Enum=ConfigMap;Secret
	// +required
	Kind string `json:"kind"`

	// Name of the referent.
	// +required
	Name string `json:"name"`

	// Optional marks the reference as non-blocking when absent.
	// +optional
	Optional bool `json:"optional,omitempty"`
}

// StageActions groups the typed steps around a stage.
type StageActions struct {
	// Pre runs before BUILD; failure aborts the stage untouched.
	// +optional
	Pre []Action `json:"pre,omitempty"`

	// Post runs after VERIFY; the stage is Ready only when these succeed.
	// +optional
	Post []Action `json:"post,omitempty"`

	// OnFailure runs best-effort on any failure from APPLY onward.
	// +optional
	OnFailure []Action `json:"onFailure,omitempty"`
}

// Action is one typed step. Exactly one of Patch, HTTP, Wait, or Job must be
// set — enforced by the validating admission webhook (and a reconciler
// fallback), not a CRD CEL rule, so spec.stages and the action lists stay
// unbounded (CEL cost is multiplied by enclosing array sizes).
type Action struct {
	// Name of the action, for status and Events.
	// +required
	Name string `json:"name"`

	// Timeout for the action.
	// +optional
	Timeout *metav1.Duration `json:"timeout,omitempty"`

	// Retries before the action is considered failed.
	// +optional
	Retries *int32 `json:"retries,omitempty"`

	// Patch modifies an existing in-cluster object.
	// +optional
	Patch *PatchAction `json:"patch,omitempty"`

	// HTTP calls an endpoint; gated by --allowed-action-hosts.
	// +optional
	HTTP *HTTPAction `json:"http,omitempty"`

	// Wait blocks for a duration or until a CEL expression holds.
	// +optional
	Wait *WaitAction `json:"wait,omitempty"`

	// Job renders and awaits Jobs from an ExternalArtifact path.
	// +optional
	Job *JobAction `json:"job,omitempty"`

	// Delete removes an existing in-cluster object — primarily for
	// migrations that recreate immutable objects. A missing target is
	// success (idempotent).
	// +optional
	Delete *DeleteAction `json:"delete,omitempty"`

	// Apply applies manifests from an ExternalArtifact path as a transient
	// action step — for resources that should live only across a rollout
	// (e.g. a maintenance-page pod), not as steady-state stage members.
	// +optional
	Apply *ApplyAction `json:"apply,omitempty"`
}

// PatchAction patches an existing in-cluster object under the impersonated
// ServiceAccount.
type PatchAction struct {
	// Target object to patch.
	// +required
	Target meta.NamespacedObjectKindReference `json:"target"`

	// Type of the patch: merge or json6902. Defaults to merge.
	// +kubebuilder:validation:Enum=merge;json6902
	// +optional
	Type string `json:"type,omitempty"`

	// Patch content.
	// +required
	Patch string `json:"patch"`
}

// HTTPAction calls an HTTP endpoint.
type HTTPAction struct {
	// URL to call; validated against --allowed-action-hosts.
	// +required
	URL string `json:"url"`

	// Method defaults to POST.
	// +optional
	Method string `json:"method,omitempty"`

	// Body inline.
	// +optional
	Body string `json:"body,omitempty"`

	// BodyFrom reads the body from a Secret key.
	// +optional
	BodyFrom *meta.SecretKeyReference `json:"bodyFrom,omitempty"`

	// HeadersFrom reads headers from Secret keys.
	// +optional
	HeadersFrom []meta.SecretKeyReference `json:"headersFrom,omitempty"`

	// ExpectedStatus codes; defaults to any 2xx.
	// +optional
	ExpectedStatus []int32 `json:"expectedStatus,omitempty"`
}

// WaitAction blocks for a duration or until a CEL expression over a
// referenced object holds.
type WaitAction struct {
	// Duration of a fixed wait.
	// +optional
	Duration *metav1.Duration `json:"duration,omitempty"`

	// Target object for Expr.
	// +optional
	Target *meta.NamespacedObjectKindReference `json:"target,omitempty"`

	// Expr is a CEL expression over the target's state.
	// +optional
	Expr string `json:"expr,omitempty"`

	// Timeout for expression-based waits.
	// +optional
	Timeout *metav1.Duration `json:"timeout,omitempty"`
}

// JobAction renders Job objects from an ExternalArtifact path, applies them
// with a run-scoped suffix, awaits completion, and garbage-collects them.
type JobAction struct {
	// SourceRef references the ExternalArtifact containing the Jobs.
	// +required
	SourceRef SourceReference `json:"sourceRef"`

	// Path inside the artifact. Defaults to "./".
	// +optional
	Path string `json:"path,omitempty"`
}

// DeleteAction removes an existing in-cluster object.
type DeleteAction struct {
	// Target object to delete.
	// +required
	Target meta.NamespacedObjectKindReference `json:"target"`
}

// ApplyAction applies the manifests built from an ExternalArtifact path under
// the run's (impersonated) client. Unlike a stage, the applied objects are NOT
// recorded in the stage inventory and are never pruned by the inventory diff —
// they persist until a delete action removes them. Use it for transient,
// rollout-scoped resources; because actions are gated by the per-revision
// ledger, the pair "apply at the first stage, delete at the last" stands the
// resource up only for the duration of a rollout. An onFailure delete guards a
// mid-run crash from orphaning it.
type ApplyAction struct {
	// SourceRef references the ExternalArtifact (directly or via a producer)
	// holding the manifests to apply.
	// +required
	SourceRef SourceReference `json:"sourceRef"`

	// Path inside the artifact to build. Defaults to "./".
	// +optional
	Path string `json:"path,omitempty"`

	// Wait blocks until the applied objects report Ready (kstatus), bounded
	// by the action Timeout — so a following patch action can repoint traffic
	// only once the resource is serving.
	// +optional
	Wait bool `json:"wait,omitempty"`
}

// Decryption configures SOPS decryption of encrypted files in stage sources.
// Decryption happens at build time, in memory; the rollback store that retains
// rendered output is encrypted at rest, so plaintext never lands on disk.
type Decryption struct {
	// Provider is the decryption backend. Only "sops" is supported.
	// +kubebuilder:validation:Enum=sops
	// +required
	Provider string `json:"provider"`

	// SecretRef names a Secret in the StageSet's namespace holding the
	// decryption keys, using the SOPS key conventions: age private keys under
	// data entries suffixed ".agekey", armored PGP private keys under ".asc".
	// The Secret is read under the StageSet's serviceAccountName, so a tenant
	// can only decrypt with material its ServiceAccount can read. Optional: omit
	// for a cloud-KMS-only setup that uses the controller's ambient credentials.
	// +optional
	SecretRef *meta.LocalObjectReference `json:"secretRef,omitempty"`
}

// VersionSource identifies the deployed system's version. Exactly one of
// Value, FromObject, or FromArtifact is set.
type VersionSource struct {
	// FromArtifact reads a single semver string from a file in a stage's
	// artifact (e.g. a VERSION file committed beside the manifests), so the
	// version moves with the content. Suited to Git/OCI/Bucket sources that can
	// carry an extra file.
	// +optional
	FromArtifact *ArtifactVersionRef `json:"fromArtifact,omitempty"`

	// FromObject reads the version from a field of a rendered object in a stage
	// — by default the standard app.kubernetes.io/version label. The version
	// travels inside the manifests themselves, so it works for every source kind
	// including a single-document renderer like JaaS, which has no room for a
	// separate version file.
	// +optional
	FromObject *ObjectVersionRef `json:"fromObject,omitempty"`

	// Value pins the version inline, for fully pin-tagged setups.
	// +optional
	Value string `json:"value,omitempty"`
}

// ObjectVersionRef reads the version from a field of one rendered object in a
// stage's built manifests, so the version travels with the content it versions.
type ObjectVersionRef struct {
	// Stage whose rendered manifests carry the version.
	// +required
	Stage string `json:"stage"`

	// Kind of the object to read (e.g. Deployment).
	// +required
	Kind string `json:"kind"`

	// Name of the object to read.
	// +required
	Name string `json:"name"`

	// APIVersion of the object; empty matches on Kind and Name alone (the
	// common case, where one Kind+Name pair is unambiguous within a stage).
	// +optional
	APIVersion string `json:"apiVersion,omitempty"`

	// FieldPath is a kubectl-style JSONPath to the version string, e.g.
	// '{.spec.template.spec.containers[0].image}'. Empty defaults to the
	// app.kubernetes.io/version label — the Kubernetes-recommended place for an
	// application's version, which well-formed manifests already set.
	// +optional
	FieldPath string `json:"fieldPath,omitempty"`
}

// ArtifactVersionRef points at a version file inside a stage's artifact.
type ArtifactVersionRef struct {
	// Stage whose artifact carries the version file.
	// +required
	Stage string `json:"stage"`

	// Path of the version file inside the artifact.
	// +required
	Path string `json:"path"`
}

// Migration is a version-gated action ladder.
type Migration struct {
	// Name of the migration, for the idempotency ledger and Events.
	// +required
	Name string `json:"name"`

	// To is the version boundary this migration crosses up to.
	// +required
	To string `json:"to"`

	// From optionally constrains the current version it applies from.
	// +optional
	From string `json:"from,omitempty"`

	// Stage anchors when the migration runs: before this stage's
	// pre-actions.
	// +required
	Stage string `json:"stage"`

	// Actions run in list order when the boundary is crossed.
	// +optional
	Actions []Action `json:"actions,omitempty"`
}

// ConflictPolicy gives per-resource answers to immutable-field conflicts.
type ConflictPolicy struct {
	// Default action for conflicts with no matching rule.
	// +kubebuilder:validation:Enum=Fail;Recreate;KeepExisting
	// +optional
	Default string `json:"default,omitempty"`

	// Rules match conflicts to actions by target.
	// +optional
	Rules []ConflictRule `json:"rules,omitempty"`
}

// ConflictRule maps a target selector to a conflict action.
type ConflictRule struct {
	// Target selects the objects this rule governs; unset fields match any.
	// +required
	Target ConflictTarget `json:"target"`

	// Action on conflict for matching objects.
	// +kubebuilder:validation:Enum=Fail;Recreate;KeepExisting
	// +required
	Action string `json:"action"`

	// AllowDataLoss is required for Recreate on PersistentVolumeClaim /
	// PersistentVolume; refused otherwise.
	// +optional
	AllowDataLoss bool `json:"allowDataLoss,omitempty"`
}

// ConflictTarget is a partial object selector; unset fields match any value.
type ConflictTarget struct {
	// +optional
	APIVersion string `json:"apiVersion,omitempty"`
	// +optional
	Kind string `json:"kind,omitempty"`
	// +optional
	Name string `json:"name,omitempty"`
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// ReadyChecks gate stage completion. Purely observational; active steps are
// actions.
type ReadyChecks struct {
	// Timeout for readiness evaluation.
	// +optional
	Timeout *metav1.Duration `json:"timeout,omitempty"`

	// DisableWait opts the stage out of the default kstatus wait.
	// +optional
	DisableWait bool `json:"disableWait,omitempty"`

	// Checks lists explicit objects evaluated with kstatus.
	// +optional
	Checks []meta.NamespacedObjectKindReference `json:"checks,omitempty"`

	// Exprs lists CEL health checks, shape-compatible with
	// kustomize-controller's healthCheckExprs.
	// +optional
	Exprs []CustomHealthCheck `json:"exprs,omitempty"`
}

// CustomHealthCheck is a CEL-based readiness rule for a group-version-kind.
type CustomHealthCheck struct {
	// APIVersion of the target kind.
	// +required
	APIVersion string `json:"apiVersion"`

	// Kind of the target.
	// +required
	Kind string `json:"kind"`

	// Current is the CEL expression for the ready state.
	// +optional
	Current string `json:"current,omitempty"`

	// InProgress is the CEL expression for the progressing state.
	// +optional
	InProgress string `json:"inProgress,omitempty"`

	// Failed is the CEL expression for the failed state.
	// +optional
	Failed string `json:"failed,omitempty"`
}

// StageSetStatus records the observed state of a StageSet.
type StageSetStatus struct {
	// ReconcileRequestStatus carries lastHandledReconcileAt: the value of
	// the reconcile.fluxcd.io/requestedAt annotation the controller most
	// recently acted on, so `flux reconcile` can tell its request was handled.
	meta.ReconcileRequestStatus `json:",inline"`

	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration is the last spec generation reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// LastAttemptedRevisions pins the artifact revisions of the current or
	// last run, keyed by "namespace/name" of the ExternalArtifact.
	// +optional
	LastAttemptedRevisions map[string]string `json:"lastAttemptedRevisions,omitempty"`

	// LastAppliedRevisions holds the revisions of the last fully
	// successful run.
	// +optional
	LastAppliedRevisions map[string]string `json:"lastAppliedRevisions,omitempty"`

	// Stages reports per-stage progress.
	// +optional
	Stages []StageStatus `json:"stages,omitempty"`

	// InventoryMode records which --inventory-mode the stored inventory of
	// this object currently satisfies; used to sequence mode migrations.
	// +optional
	InventoryMode string `json:"inventoryMode,omitempty"`

	// Version is the currently deployed system version, written only after a
	// fully successful run.
	// +optional
	Version string `json:"version,omitempty"`

	// PendingMigrations lists the migrations the next run will execute, so
	// operators see unusual work before it happens.
	// +optional
	PendingMigrations []string `json:"pendingMigrations,omitempty"`

	// ExecutedMigrations is the in-flight migration ledger: the names of
	// migrations already completed for the current version transition. It
	// lets a retry of a partially-applied transition skip finished
	// migrations, and is cleared once status.version reaches the target.
	// +optional
	ExecutedMigrations []string `json:"executedMigrations,omitempty"`

	// LastAppliedSnapshot records, per stage, the artifact coordinates of the
	// last fully-successful run. rollbackOnFailure re-fetches these
	// revisions and re-renders them under the current spec — a pointer to the
	// producer's immutable, revision-addressed content rather than a copy of
	// the rendered output (which would reintroduce Helm's release-size limit).
	// +optional
	LastAppliedSnapshot []StageArtifactRef `json:"lastAppliedSnapshot,omitempty"`

	// PendingUpdate is set when an update window is holding a rollout (or a
	// hard freeze is active); cleared once an update applies.
	// +optional
	PendingUpdate *PendingUpdate `json:"pendingUpdate,omitempty"`

	// LastHandledUpdateOverride is the value of the most recently honored
	// stages.metio.wtf/update-now annotation, so an override fires once.
	// +optional
	LastHandledUpdateOverride string `json:"lastHandledUpdateOverride,omitempty"`
}

// PendingUpdate describes a rollout held by an update window.
type PendingUpdate struct {
	// Revisions held back, keyed by "namespace/name" of the ExternalArtifact.
	// Empty when a hard freeze pauses an otherwise steady reconcile.
	// +optional
	Revisions map[string]string `json:"revisions,omitempty"`

	// NextWindowOpens is when delivery may resume.
	// +optional
	NextWindowOpens *metav1.Time `json:"nextWindowOpens,omitempty"`
}

// StageArtifactRef pins a stage's resolved artifact so a later run can re-fetch
// exactly that revision (digest-verified).
type StageArtifactRef struct {
	// Stage name this artifact backed.
	// +required
	Stage string `json:"stage"`

	// URL of the artifact tarball.
	// +required
	URL string `json:"url"`

	// Digest (algo:hex) the tarball must match on re-fetch.
	// +required
	Digest string `json:"digest"`

	// Revision is the human-readable revision identifier.
	// +optional
	Revision string `json:"revision,omitempty"`

	// SubstitutionDigest is a sha256 fingerprint over the resolved postBuild
	// substitution inputs used for this stage's last good apply. It is a digest
	// only — it carries no substituteFrom secret VALUES, so it is safe in
	// status. On rollback the controller re-resolves substitution and, if the
	// fingerprint no longer matches, refuses to restore (terminal) rather than
	// silently re-rendering the old artifact with changed inputs.
	// +optional
	SubstitutionDigest string `json:"substitutionDigest,omitempty"`
}

// StagePhase enumerates the lifecycle phases of a stage.
// +kubebuilder:validation:Enum=Pending;Applying;Pruning;Verifying;Ready;Failed
type StagePhase string

// Stage lifecycle phases.
const (
	StagePending   StagePhase = "Pending"
	StageApplying  StagePhase = "Applying"
	StagePruning   StagePhase = "Pruning"
	StageVerifying StagePhase = "Verifying"
	StageReady     StagePhase = "Ready"
	StageFailed    StagePhase = "Failed"
)

// StageStatus reports the progress of one stage.
type StageStatus struct {
	// Name of the stage.
	// +required
	Name string `json:"name"`

	// Phase of the stage.
	// +optional
	Phase StagePhase `json:"phase,omitempty"`

	// AppliedRevision is the artifact revision last applied by this stage.
	// +optional
	AppliedRevision string `json:"appliedRevision,omitempty"`

	// EntriesCount is the number of inventory entries the stage owns.
	// +optional
	EntriesCount int64 `json:"entriesCount,omitempty"`

	// Shards is the number of StageInventory shards backing the stage.
	// +optional
	Shards int32 `json:"shards,omitempty"`

	// Message carries a human-readable progress or failure detail.
	// +optional
	Message string `json:"message,omitempty"`

	// ExecutedActions lists the action names already run at LedgerRevision, so
	// retries and controller restarts never re-fire a side effect for the same
	// pinned snapshot.
	// +optional
	ExecutedActions []string `json:"executedActions,omitempty"`

	// LedgerRevision is the pinned artifact revision the ExecutedActions ledger
	// applies to; a new revision resets the ledger.
	// +optional
	LedgerRevision string `json:"ledgerRevision,omitempty"`

	// LastHandledReconcileAt is the value of the stages.metio.wtf/reconcile-stage
	// token this stage most recently acted on. A single-stage force-reconcile
	// clears this stage's action ledger and re-runs its actions exactly once per
	// new token, the per-stage analogue of lastHandledReconcileAt.
	// +optional
	LastHandledReconcileAt string `json:"lastHandledReconcileAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Status",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].message`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// StageSet deploys an ordered, gated list of stages built from
// ExternalArtifact sources.
type StageSet struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec StageSetSpec `json:"spec,omitempty"`
	// +optional
	Status StageSetStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// StageSetList contains a list of StageSet.
type StageSetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []StageSet `json:"items"`
}
