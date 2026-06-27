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
	// Requires spec.version. Mutually exclusive with MigrationsSourceRef.
	// +optional
	Migrations []Migration `json:"migrations,omitempty"`

	// MigrationsSourceRef sources the migration ladder from a Flux source's
	// artifact instead of inlining it, so one ladder can be authored once and
	// shared across many StageSets. The artifact holds a serialized
	// []Migration. Mutually exclusive with Migrations; requires spec.version.
	// +optional
	MigrationsSourceRef *MigrationsSource `json:"migrationsSourceRef,omitempty"`

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

	// ErrorBudget freezes new-revision rollouts while a service is out of its SLO
	// error budget — the Google SRE error-budget policy. It reads one scalar
	// (budget remaining, 0..1) from a metric source and holds the rollout while
	// the value is below freezeThreshold, resuming on its own when it recovers.
	// Combined with updateWindows under a logical AND: a rollout proceeds only if
	// every gate allows.
	// +optional
	ErrorBudget *ErrorBudget `json:"errorBudget,omitempty"`

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

// ErrorBudget configures a rollout-wide error-budget freeze. The source's
// scalar is the remaining error budget (1 = full, 0 = exhausted, <0 =
// overspent); the rollout freezes while it is below freezeThreshold and resumes
// once it reaches resumeThreshold.
type ErrorBudget struct {
	// Source resolves to the remaining error budget as a scalar (typically a
	// 0..1 ratio). The controller carries no SLO math — it reads this number and
	// compares it to the thresholds below.
	// +required
	Source MetricSource `json:"source"`

	// FreezeThreshold freezes new-revision rollouts when the remaining budget is
	// strictly below this value. "0" freezes only when the budget is overspent;
	// "0.1" keeps a 10% reserve. A decimal string.
	// +required
	FreezeThreshold string `json:"freezeThreshold"`

	// ResumeThreshold resumes rollouts only once the remaining budget reaches
	// this value. Set it above freezeThreshold for hysteresis (freeze at 0,
	// resume at 0.05) so a budget hovering at the threshold doesn't flap the
	// freeze. Defaults to freezeThreshold (no hysteresis). A decimal string.
	// +optional
	ResumeThreshold string `json:"resumeThreshold,omitempty"`

	// OnSourceError chooses what happens when the source is unreachable or
	// returns no usable scalar: Allow (proceed — the default, because blocking a
	// rollout-wide freeze stops every deploy including the hotfix you need during
	// the very outage that took the source down) or Hold (block). Either way the
	// error is loud: a BudgetSourceUnavailable condition, a Warning event, and
	// the metric_source_errors metric.
	// +kubebuilder:validation:Enum=Allow;Hold
	// +optional
	OnSourceError string `json:"onSourceError,omitempty"`

	// Interval is the re-check cadence while frozen. Defaults to the StageSet's
	// reconcile interval.
	// +optional
	Interval *metav1.Duration `json:"interval,omitempty"`

	// DryRun records what would freeze (status + metric) without holding the
	// rollout, so an operator can prove a freeze rule fires before it gates.
	// +optional
	DryRun bool `json:"dryRun,omitempty"`
}

// Stage is one ordered, gated unit of a StageSet.
type Stage struct {
	// Name of the stage; unique within the StageSet, used as inventory key.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +required
	Name string `json:"name"`

	// MigrationAnchor declares a stable anchor role this stage fulfils, so a
	// shared migration ladder can anchor to it by role rather than by this
	// stage's Name. A migration's Stage value resolves to the stage whose
	// MigrationAnchor (preferred) or Name matches it. Anchor keys must be
	// unique across stage Names and MigrationAnchors within a StageSet.
	// +optional
	MigrationAnchor string `json:"migrationAnchor,omitempty"`

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

	// Promotion gates advancement from this stage to the next one. Unlike
	// ReadyChecks (which decide whether the just-applied objects became Ready),
	// a promotion gate decides whether a healthy, applied stage is allowed to
	// advance the rollout. Holding a promotion gate parks the rollout at this
	// stage — the stage stays applied and its drift keeps being corrected — and
	// never touches later stages.
	// +optional
	Promotion *StagePromotion `json:"promotion,omitempty"`

	// ErrorBudget freezes a NEW revision from rolling into THIS stage while the
	// stage's own SLO error budget is exhausted — the per-stage analogue of
	// spec.errorBudget. It gates entry (before this stage applies); the stage's
	// currently-applied revision keeps having its drift corrected, and earlier
	// stages are unaffected. Use it to freeze only a sensitive stage (e.g. prod)
	// on that environment's budget while earlier stages keep rolling. This gates
	// entry; promotion.analysis gates exit (advancing past a stage once applied).
	// +optional
	ErrorBudget *ErrorBudget `json:"errorBudget,omitempty"`
}

// StagePromotion gates advancement past a stage. Every mechanism is optional;
// with none set the stage advances as soon as it is Ready (the default).
type StagePromotion struct {
	// Soak holds the rollout at this stage for the given duration after it
	// becomes Ready, advancing only if it stays healthy for the whole window.
	// This catches delayed regressions (OOM after warm-up, error-rate creep,
	// crashloop-after-N-minutes) that point-in-time ReadyChecks miss. The soak
	// gates only advancement to the next stage; drift on this stage's applied
	// revision keeps being corrected during the window. Defaults to no soak.
	// +optional
	Soak *metav1.Duration `json:"soak,omitempty"`

	// RequireManualPromotion holds the rollout at this stage until an operator
	// promotes it with `stagesetctl promote NAME --stage <name>` (which stamps
	// the stages.metio.wtf/promote annotation). Kept distinct from a migration's
	// RequireApproval (which gates a version transition, not a stage advance).
	// Defaults to false.
	// +optional
	RequireManualPromotion bool `json:"requireManualPromotion,omitempty"`

	// Analysis advances the stage only if metric checks against external sources
	// keep passing. Each check reads one scalar and compares it to a threshold;
	// a stage is promoted once its checks pass and refused after more than
	// failureLimit failing evaluations. This sees behavior a Deployment's own
	// .status cannot — downstream error rate, latency, SLO burn — that
	// point-in-time ReadyChecks miss. Combine with soak to observe over a window.
	// +optional
	Analysis *PromotionAnalysis `json:"analysis,omitempty"`

	// FastTrack shortens the soak when the system is demonstrably healthy: once
	// the minimum soak has elapsed and a burn-rate (or similar) metric is within
	// its bound, the stage promotes early instead of waiting out the full soak.
	// It only ever promotes EARLIER than soak — it never blocks past it (blocking
	// on a metric is promotion.analysis's job). Requires soak.
	// +optional
	FastTrack *FastTrack `json:"fastTrack,omitempty"`

	// RestartGate blocks promotion when a watched group of pods restarts too often
	// while the stage soaks. It catches a crashloop or an OOM-after-warm-up that
	// leaves the workload Ready (so ReadyChecks pass) while individual pods keep
	// restarting — the dependency-free companion to a soak.
	// +optional
	RestartGate *RestartGate `json:"restartGate,omitempty"`

	// EventGate blocks promotion when a watched group of pods accumulates too many
	// Warning events (FailedScheduling, OOMKilling, ImagePullBackOff, FailedMount,
	// …) while the stage soaks. It surfaces behaviour that leaves the workload
	// Ready — so ReadyChecks pass and pods may not even restart — yet the rollout
	// is sick; a companion to the restart gate.
	// +optional
	EventGate *EventGate `json:"eventGate,omitempty"`
}

// RestartGate watches one or more pod groups for restarts during a stage's soak;
// a breach in any check blocks the stage's promotion.
type RestartGate struct {
	// OnFailure is the default action when a check breaches: Hold (the default)
	// parks the rollout at this stage and surfaces why; Rollback reverts the stage
	// to its last-good revision — reusing spec.rollbackOnFailure's snapshots,
	// scoped to this stage — and parks the failing revision so it isn't re-applied
	// each reconcile. With no snapshot available a rollback degrades to a hold. A
	// check may override this with its own onFailure.
	// +optional
	// +kubebuilder:validation:Enum=Hold;Rollback
	OnFailure string `json:"onFailure,omitempty"`

	// Checks are the pod groups to watch; a breach in any one blocks promotion.
	// Set several (e.g. an API and a worker) with independent tolerances.
	// +required
	// +listType=map
	// +listMapKey=name
	Checks []RestartCheck `json:"checks"`
}

// RestartCheck blocks a stage's promotion when one group of pods restarts more
// than maxRestarts times. Pods are selected by label (in the StageSet's
// namespace), so the group is not tied to a single workload kind — it can span
// Deployments, StatefulSets, DaemonSets, Jobs, or a custom controller. The pods
// must be observable by the controller (the same cluster, or the one the stage's
// kubeConfig targets).
type RestartCheck struct {
	// Name identifies the check in status, events, and the Ready message. Unique
	// within a stage's promotion.
	// +required
	Name string `json:"name"`

	// Selector chooses the pods this check watches, in the StageSet's namespace.
	// It must match at least one label; an empty selector (which would match every
	// pod) is rejected.
	// +required
	Selector metav1.LabelSelector `json:"selector"`

	// MaxRestarts is the total container restarts — summed across the selected
	// pods' init and regular containers — tolerated before the check fails.
	// Defaults to 0: no restarts allowed.
	// +optional
	// +kubebuilder:validation:Minimum=0
	MaxRestarts int32 `json:"maxRestarts,omitempty"`

	// OnFailure overrides the gate's default action for this check (Hold or
	// Rollback). Defaults to the RestartGate's onFailure.
	// +optional
	// +kubebuilder:validation:Enum=Hold;Rollback
	OnFailure string `json:"onFailure,omitempty"`
}

// EventGate watches one or more pod groups for Warning events during a stage's
// soak; a breach in any check blocks the stage's promotion.
type EventGate struct {
	// OnFailure is the default action when a check breaches: Hold (default) or
	// Rollback. Identical semantics to RestartGate.onFailure; a check may override.
	// +optional
	// +kubebuilder:validation:Enum=Hold;Rollback
	OnFailure string `json:"onFailure,omitempty"`

	// Checks are the pod groups to watch; a breach in any one blocks promotion.
	// +required
	// +listType=map
	// +listMapKey=name
	Checks []EventCheck `json:"checks"`
}

// EventCheck blocks a stage's promotion when its pods accumulate more than
// maxEvents Warning events carrying one of the named reasons. Pods are selected
// by label (in the StageSet's namespace), so the group is not tied to a single
// workload kind. The pods must be observable by the controller (the same cluster,
// or the one the stage's kubeConfig targets), which needs events list access.
type EventCheck struct {
	// Name identifies the check in status, events, and the Ready message. Unique
	// within a stage's promotion.
	// +required
	Name string `json:"name"`

	// Selector chooses the pods this check watches, in the StageSet's namespace.
	// It must match at least one label; an empty selector is rejected.
	// +required
	Selector metav1.LabelSelector `json:"selector"`

	// Reasons is the set of Warning event reasons that count — e.g.
	// FailedScheduling, OOMKilling, BackOff, FailedMount, ErrImagePull. Required:
	// events are noisy, so a check counts only the reasons you name and ignores the
	// rest.
	// +required
	// +kubebuilder:validation:MinItems=1
	Reasons []string `json:"reasons"`

	// MaxEvents is the total matching Warning events — summed by occurrence count
	// across the selected pods — tolerated before the check fails. Defaults to 0:
	// any matching event fails.
	// +optional
	// +kubebuilder:validation:Minimum=0
	MaxEvents int32 `json:"maxEvents,omitempty"`

	// OnFailure overrides the gate's default action for this check (Hold or
	// Rollback). Defaults to the EventGate's onFailure.
	// +optional
	// +kubebuilder:validation:Enum=Hold;Rollback
	OnFailure string `json:"onFailure,omitempty"`
}

// FastTrack accelerates a soak based on a metric. After the minimum soak, if the
// metric stays at or below max, the stage promotes without waiting the full soak.
type FastTrack struct {
	// Source resolves to the scalar gating early promotion — typically a
	// burn-rate ratio (e.g. Sloth's slo:current_burn_rate:ratio).
	// +required
	Source MetricSource `json:"source"`

	// Max is the highest value that still allows early promotion (a decimal
	// string). For a burn rate, "1" means "not burning faster than sustainable".
	// +required
	Max string `json:"max"`

	// After is the minimum soak that must elapse before early promotion is
	// considered, so a stage always soaks at least this long. Defaults to 0 (a
	// healthy metric can promote immediately).
	// +optional
	After *metav1.Duration `json:"after,omitempty"`
}

// PromotionAnalysis evaluates metric checks at a stage boundary. Every check
// must stay within its threshold; the stage is refused after more than
// failureLimit consecutive failing evaluations.
type PromotionAnalysis struct {
	// Checks are the metric comparisons, all of which must pass for the stage to
	// promote. Each is a metric source plus a threshold.
	// +kubebuilder:validation:MinItems=1
	// +required
	Checks []AnalysisCheck `json:"checks"`

	// Interval is the re-evaluation cadence while the analysis holds the rollout.
	// Defaults to the StageSet's reconcile interval.
	// +optional
	Interval *metav1.Duration `json:"interval,omitempty"`

	// FailureLimit is how many consecutive failing evaluations are tolerated
	// before the analysis fails the promotion. The count resets on a passing
	// evaluation. Defaults to 0 (any failing evaluation fails the promotion).
	// +kubebuilder:validation:Minimum=0
	// +optional
	FailureLimit *int32 `json:"failureLimit,omitempty"`

	// OnFailure chooses what happens when the analysis fails: Hold (default —
	// leave the stage applied but not promoted, surfacing why) or Rollback
	// (revert this stage to its last-known-good revision, reusing the
	// rollbackOnFailure snapshot machinery; scoped to this stage only).
	// +kubebuilder:validation:Enum=Hold;Rollback
	// +optional
	OnFailure string `json:"onFailure,omitempty"`

	// OnSourceError chooses what happens when a check's source is unreachable:
	// Hold (default — never advance a stage whose behavior can't be verified;
	// safe because holding only parks the rollout at the current healthy stage)
	// or Allow (proceed). The opposite default of the error-budget freeze, which
	// fails open because blocking it would stop every deploy.
	// +kubebuilder:validation:Enum=Hold;Allow
	// +optional
	OnSourceError string `json:"onSourceError,omitempty"`

	// DryRun records what would block the promotion (status + metric) without
	// holding the rollout, so an operator can prove an analysis rule fires before
	// it gates.
	// +optional
	DryRun bool `json:"dryRun,omitempty"`
}

// AnalysisCheck is one metric comparison in a promotion analysis.
type AnalysisCheck struct {
	// Name identifies the check in status and events.
	// +required
	Name string `json:"name"`

	// Source resolves to the scalar this check compares.
	// +required
	Source MetricSource `json:"source"`

	// Threshold the scalar must stay within (min/max). At least one bound is
	// required.
	// +required
	Threshold Threshold `json:"threshold"`
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

// Action is one typed step. Exactly one of Patch, HTTP, Wait, Job, Delete, or
// Apply must be set — enforced by the validating admission webhook (and a
// reconciler fallback), not a CRD CEL rule, so spec.stages and the action lists
// stay unbounded (CEL cost is multiplied by enclosing array sizes).
type Action struct {
	// Name of the action, for status and Events.
	// +required
	Name string `json:"name"`

	// Timeout for the action.
	// +optional
	Timeout *metav1.Duration `json:"timeout,omitempty"`

	// Retries before the action is considered failed.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
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

// Verb returns the action's operation type ("patch", "http", "wait", "job",
// "delete", "apply"), or "action" when none is set. It names the single
// operation the action performs.
func (a *Action) Verb() string {
	switch {
	case a.Patch != nil:
		return "patch"
	case a.HTTP != nil:
		return "http"
	case a.Wait != nil:
		return "wait"
	case a.Job != nil:
		return "job"
	case a.Delete != nil:
		return "delete"
	case a.Apply != nil:
		return "apply"
	default:
		return "action"
	}
}

// VerbCount returns how many operation fields the action sets. Exactly one is
// valid; the admission webhook and the migration validators enforce it.
func (a *Action) VerbCount() int {
	n := 0
	for _, set := range []bool{a.Patch != nil, a.HTTP != nil, a.Wait != nil, a.Job != nil, a.Delete != nil, a.Apply != nil} {
		if set {
			n++
		}
	}
	return n
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
	// +kubebuilder:validation:Enum=GET;POST;PUT;PATCH;DELETE;HEAD
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
	// +kubebuilder:validation:items:Minimum=100
	// +kubebuilder:validation:items:Maximum=599
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

	// RequireMigrationCoverage fails the reconcile when a version transition
	// crosses a major-version boundary with no migration covering it, instead of
	// advancing the major change silently. Off by default.
	// +optional
	RequireMigrationCoverage bool `json:"requireMigrationCoverage,omitempty"`

	// RequireApproval holds a version transition that has pending migrations
	// until an operator approves the target version with the
	// stages.metio.wtf/approved-version annotation, so destructive migrations
	// don't run unattended. Off by default.
	// +optional
	RequireApproval bool `json:"requireApproval,omitempty"`
}

// ObjectVersionRef reads the version from a field of one rendered object in a
// stage's built manifests, so the version travels with the content it versions.
type ObjectVersionRef struct {
	// Stage whose rendered manifests carry the version. A StageSet has one
	// version that all its stages converge on, so this only names which stage's
	// output to read it from — normally the leading stage, which carries the new
	// version first. Empty defaults to the first stage.
	// +optional
	Stage string `json:"stage,omitempty"`

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

	// To is the exact version boundary this migration crosses up to (a
	// concrete semver, not a range).
	// +required
	To string `json:"to"`

	// From optionally constrains the current version it applies from. Unlike
	// To, this is a semver *constraint* (e.g. ">=1.0.0, <2.0.0" or "1.x"),
	// matched against the currently deployed version. Omit to fire on every
	// crossing up to To.
	// +optional
	From string `json:"from,omitempty"`

	// Stage anchors when the migration runs: before the pre-actions of the
	// stage whose MigrationAnchor (preferred) or Name equals this value. Omit
	// to anchor before the first stage. A value matching no stage in a
	// consuming StageSet fails closed (reason MigrationStageNotFound) rather
	// than silently skipping — important for a ladder shared across StageSets
	// whose stages may be named differently.
	// +optional
	Stage string `json:"stage,omitempty"`

	// Actions run in list order when the boundary is crossed.
	// +optional
	Actions []Action `json:"actions,omitempty"`
}

// MigrationsSource references a Flux source whose artifact holds a serialized
// []Migration ladder, letting the ladder be authored once and shared across
// many StageSets instead of inlined into each. Used by spec.migrationsSourceRef.
type MigrationsSource struct {
	// SourceRef references the artifact-producing source (an ExternalArtifact,
	// or a producer kind resolved via the back-pointer index — the same
	// reference shape a stage uses). Resolved in the StageSet's namespace;
	// cross-namespace references are gated by --no-cross-namespace-refs.
	// +required
	SourceRef SourceReference `json:"sourceRef"`

	// Path inside the artifact selecting the migrations file or directory.
	// Defaults to the artifact root.
	// +optional
	Path string `json:"path,omitempty"`
}

// PendingMigration is a status preview of a migration the next run will execute:
// its boundary, the concrete stage it anchors before, the action verbs it runs,
// and its content digest — so operators see what is about to happen even when the
// ladder is sourced from an artifact rather than the spec.
type PendingMigration struct {
	// Name of the migration.
	Name string `json:"name"`
	// To is the version boundary this migration crosses up to.
	To string `json:"to"`
	// From is the optional version constraint it applies from.
	// +optional
	From string `json:"from,omitempty"`
	// Stage is the concrete stage this migration resolved to anchor before.
	// +optional
	Stage string `json:"stage,omitempty"`
	// Actions lists the action verbs (e.g. delete, apply, job) in run order.
	// +optional
	Actions []string `json:"actions,omitempty"`
	// Digest is the migration's resolved content digest, the idempotency-ledger key.
	// +optional
	Digest string `json:"digest,omitempty"`
}

// ConflictPolicy gives per-resource answers to immutable-field conflicts.
type ConflictPolicy struct {
	// Default action for conflicts with no matching rule.
	// +kubebuilder:validation:Enum=Fail;Recreate;KeepExisting
	// +kubebuilder:default=Fail
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

	// Version is the currently deployed system version, written only after a
	// fully successful run.
	// +optional
	Version string `json:"version,omitempty"`

	// PendingMigrations lists the migrations the next run will execute, with
	// enough detail (boundary, resolved anchor stage, action verbs, content
	// digest) that operators see exactly what destructive work is about to run
	// — including for a ladder sourced from spec.migrationsSourceRef, whose
	// content is not in the spec.
	// +optional
	PendingMigrations []PendingMigration `json:"pendingMigrations,omitempty"`

	// ExecutedMigrations is the in-flight migration ledger: the
	// "name@contentDigest" keys of migrations fully completed for the current
	// version transition. Keying on the content digest means an edited
	// migration (same name, changed content) is treated as a new, unexecuted
	// migration rather than silently skipped. Lets a retry of a
	// partially-applied transition skip finished migrations; cleared once
	// status.version reaches the target.
	// +optional
	ExecutedMigrations []string `json:"executedMigrations,omitempty"`

	// ExecutedMigrationActions is the per-action migration ledger:
	// "name@contentDigest/actionName" keys of individual migration actions
	// already completed. It lets a retry of a partially-applied migration skip
	// actions that already ran, so a destructive action is never re-executed
	// after a mid-migration failure. Cleared with ExecutedMigrations once
	// status.version reaches the target.
	// +optional
	ExecutedMigrationActions []string `json:"executedMigrationActions,omitempty"`

	// MigrationFailureCount counts consecutive migration failures in the current
	// transition. Once it reaches the controller's threshold the migration goes
	// MigrationDirty (auto-retry halts). Reset to zero on a successful transition
	// or a manual reconcile request.
	// +optional
	MigrationFailureCount int32 `json:"migrationFailureCount,omitempty"`

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

	// BudgetFreeze is set while an error-budget freeze holds new-revision
	// rollouts (or, under dryRun, while a freeze would hold them). It surfaces the
	// last observed remaining budget so a wrong query doesn't hide.
	// +optional
	BudgetFreeze *BudgetFreeze `json:"budgetFreeze,omitempty"`

	// LastHandledBudgetOverride is the value of the most recently honored
	// stages.metio.wtf/budget-override annotation, so an override fires once.
	// +optional
	LastHandledBudgetOverride string `json:"lastHandledBudgetOverride,omitempty"`
}

// BudgetFreeze reports an active (or, under dryRun, simulated) error-budget
// freeze: the last observed remaining budget, the thresholds in effect, when the
// freeze began, and whether it is a dry run.
type BudgetFreeze struct {
	// Remaining is the last observed remaining-budget scalar, as a decimal
	// string, so an operator can see whether the source returns what they expect.
	// +optional
	Remaining string `json:"remaining,omitempty"`

	// FreezeThreshold echoes the threshold the freeze tripped at.
	// +optional
	FreezeThreshold string `json:"freezeThreshold,omitempty"`

	// ResumeThreshold echoes the threshold the freeze will resume at.
	// +optional
	ResumeThreshold string `json:"resumeThreshold,omitempty"`

	// Since is when the freeze began.
	// +optional
	Since *metav1.Time `json:"since,omitempty"`

	// DryRun is true when the freeze is recorded but not enforced.
	// +optional
	DryRun bool `json:"dryRun,omitempty"`
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

	// PromotionState reports where this stage stands against its promotion gate
	// (soak / manual). Absent when the stage has no promotion gate.
	// +optional
	PromotionState *PromotionState `json:"promotionState,omitempty"`

	// LastHandledPromotion is the stages.metio.wtf/promote token this stage most
	// recently acted on, so a manual promotion fires exactly once per new token.
	// +optional
	LastHandledPromotion string `json:"lastHandledPromotion,omitempty"`

	// BudgetFreeze is set while this stage's own errorBudget is holding a
	// new-revision entry into the stage (or, under dryRun, would hold it).
	// +optional
	BudgetFreeze *BudgetFreeze `json:"budgetFreeze,omitempty"`
}

// PromotionPhase is where a stage stands against its promotion gate.
type PromotionPhase string

const (
	// PromotionSoaking means the stage is applied and healthy and the rollout is
	// holding through its soak window before advancing.
	PromotionSoaking PromotionPhase = "Soaking"
	// PromotionAnalyzing means the soak has elapsed (or none is set) and the
	// rollout is holding while a promotion analysis observes the stage's metrics.
	PromotionAnalyzing PromotionPhase = "Analyzing"
	// PromotionBlocked means a promotion analysis has failed past its
	// failureLimit and the stage is held (onFailure=Hold).
	PromotionBlocked PromotionPhase = "Blocked"
	// PromotionAwaitingManual means the stage is waiting for an operator to
	// promote it (RequireManualPromotion).
	PromotionAwaitingManual PromotionPhase = "AwaitingManual"
	// PromotionPromoted means the stage cleared its promotion gate and the
	// rollout has advanced past it.
	PromotionPromoted PromotionPhase = "Promoted"
)

// PromotionState reports a stage's promotion-gate progress. It is keyed to the
// stage's applied revision: a new revision restarts the soak from scratch.
type PromotionState struct {
	// Phase is the current promotion phase.
	// +optional
	Phase PromotionPhase `json:"phase,omitempty"`

	// Since is when the current phase began. For Soaking it marks the soak
	// start, so the soak deadline is Since + spec soak.
	// +optional
	Since *metav1.Time `json:"since,omitempty"`

	// SoakUntil is the instant the soak window closes (Soaking only).
	// +optional
	SoakUntil *metav1.Time `json:"soakUntil,omitempty"`

	// AnalysisFailures is the running count of consecutive failing analysis
	// evaluations; it resets to zero on a passing evaluation and trips onFailure
	// once it exceeds failureLimit.
	// +optional
	AnalysisFailures int32 `json:"analysisFailures,omitempty"`

	// RestartCheck names the promotion.restartChecks entry whose pods exceeded
	// their maxRestarts, when a restart breach is what blocks the rollout.
	// +optional
	RestartCheck string `json:"restartCheck,omitempty"`

	// ObservedRestarts is the restart total the breaching RestartCheck observed.
	// +optional
	ObservedRestarts int32 `json:"observedRestarts,omitempty"`

	// EventCheck names the promotion.eventGate check whose pods exceeded their
	// maxEvents, when a Warning-event breach is what blocks the rollout.
	// +optional
	EventCheck string `json:"eventCheck,omitempty"`

	// ObservedEvents is the matching Warning-event total the breaching EventCheck
	// observed.
	// +optional
	ObservedEvents int32 `json:"observedEvents,omitempty"`

	// AbortedRevision names the revision a promotion analysis with
	// onFailure=Rollback reverted away from. While it equals the pinned revision
	// the rollout stays reverted and the stage is not re-applied — so a failing
	// revision isn't churned apply→fail→revert each reconcile. Cleared by a new
	// revision or a manual promote.
	// +optional
	AbortedRevision string `json:"abortedRevision,omitempty"`

	// LastAnalysis records the most recent promotion-analysis evaluation, so an
	// operator sees each check's observed value and verdict.
	// +optional
	LastAnalysis *AnalysisResult `json:"lastAnalysis,omitempty"`
}

// AnalysisResult is the outcome of one promotion-analysis evaluation.
type AnalysisResult struct {
	// Time the evaluation ran.
	// +optional
	Time *metav1.Time `json:"time,omitempty"`

	// Passed is true when every check stayed within its threshold.
	// +optional
	Passed bool `json:"passed,omitempty"`

	// Checks reports each check's observed value and verdict.
	// +optional
	Checks []AnalysisCheckResult `json:"checks,omitempty"`
}

// AnalysisCheckResult is one check's observed value and verdict.
type AnalysisCheckResult struct {
	// Name of the check.
	// +required
	Name string `json:"name"`

	// Value is the observed scalar, as a decimal string. Empty when the source
	// could not be read.
	// +optional
	Value string `json:"value,omitempty"`

	// OK is true when the value stayed within the check's threshold.
	// +optional
	OK bool `json:"ok,omitempty"`

	// Error describes why the source could not be read, when applicable.
	// +optional
	Error string `json:"error,omitempty"`
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

// GetConditions returns the status conditions of the StageSet. It satisfies
// the conditions.Setter/Getter contract the Flux pkg/runtime condition and
// patch helpers expect. The methods deal only in apimachinery's
// metav1.Condition, so the API package takes no dependency on the
// controller-runtime or Flux condition packages.
func (in *StageSet) GetConditions() []metav1.Condition {
	return in.Status.Conditions
}

// SetConditions replaces the status conditions of the StageSet.
func (in *StageSet) SetConditions(conditions []metav1.Condition) {
	in.Status.Conditions = conditions
}

// +kubebuilder:object:root=true

// StageSetList contains a list of StageSet.
type StageSetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []StageSet `json:"items"`
}
