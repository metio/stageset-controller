// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

// ConditionReady is the StageSet's summary Ready condition type, matching the
// Flux convention every notification-controller alert and dashboard keys on.
const ConditionReady = "Ready"

// FinalizerName guards a StageSet so its applied objects are torn down (in
// reverse stage order) before the object is removed. Wire-stable.
const FinalizerName = "stages.metio.wtf/finalizer"

// Wire-stable Ready-condition reasons. notification-controller routing and
// operator dashboards match on these strings, so renaming any is a breaking
// change.
const (
	// ReasonSuspended: spec.suspend is set; reconciliation is paused.
	ReasonSuspended = "Suspended"

	// ReasonInvalidSpec: the spec is invalid (e.g. an action does not set
	// exactly one of patch/http/wait/job) and admission did not reject it.
	// Terminal until the spec is fixed.
	ReasonInvalidSpec = "InvalidSpec"

	// ReasonSourceNotReady: a stage's ExternalArtifact exists but is not yet
	// Ready (its producer has not published). Transient.
	ReasonSourceNotReady = "SourceNotReady"

	// ReasonArtifactNotFound: a stage's sourceRef (direct or producer
	// back-pointer) resolves to no ExternalArtifact.
	ReasonArtifactNotFound = "ArtifactNotFound"

	// ReasonResolveFailed: a stage's sourceRef could not be resolved for a
	// spec/config reason (ambiguous producer, cross-namespace rejected) or an
	// API error.
	ReasonResolveFailed = "ResolveFailed"

	// ReasonDependencyNotReady: a spec.dependsOn StageSet is not Ready at its
	// observed generation. Transient.
	ReasonDependencyNotReady = "DependencyNotReady"

	// ReasonStalled: a terminal condition that will not clear without a spec
	// change (e.g. a dependsOn cycle).
	ReasonStalled = "Stalled"

	// ReasonInvalidVersion: spec.version.fromArtifact points at a missing or
	// unparseable version file. Terminal — a half-versioned system is worse
	// than an unversioned one.
	ReasonInvalidVersion = "InvalidVersion"

	// ReasonDowngradeRequiresMigration: the desired version is lower than
	// status.version. Refused by default — replaying upgrade migrations in
	// reverse is how data dies. Terminal until the version moves forward.
	ReasonDowngradeRequiresMigration = "DowngradeRequiresMigration"

	// ReasonMigrationStageNotFound: a version-selected migration anchors to a
	// value matching no stage Name or MigrationAnchor in this StageSet. Fails
	// closed — the version is NOT advanced — rather than silently never running
	// a destructive migration (the failure mode a shared, late-binding ladder
	// must not have). Terminal until the anchor or the stages are fixed.
	ReasonMigrationStageNotFound = "MigrationStageNotFound"

	// ReasonMigrationArtifactInvalid: a migration ladder sourced via
	// spec.migrationsSourceRef could not be parsed or failed content validation
	// (no migration files, malformed YAML/JSON, a migration with an empty
	// name/to, a malformed action, or a duplicate migration name). Terminal
	// until the artifact is fixed and republished.
	ReasonMigrationArtifactInvalid = "MigrationArtifactInvalid"

	// ReasonMigrationSourceNotVerified: a sourced migration ladder's source is
	// not signature-verified — its verification FAILED (SourceVerified=False),
	// or --require-verified-migration-sources is set and the source configures
	// no verification at all. Fails closed: unverified destructive instructions
	// are not executed. Terminal until the source's spec.verify passes.
	ReasonMigrationSourceNotVerified = "MigrationSourceNotVerified"

	// ReasonMigrationSourceNotPinned: --require-pinned-migration-sources is set
	// and a sourced migration ladder's source is pinned to a mutable tag/branch
	// rather than an immutable digest/commit, so an upstream overwrite could
	// auto-roll new destructive content. Terminal until the source is pinned.
	ReasonMigrationSourceNotPinned = "MigrationSourceNotPinned"

	// ReasonMigrationFailed: a migration's action failed this reconcile. Distinct
	// from ReasonStageFailed so operators can tell a stage's own apply from the
	// migration anchored before it. Retries with backoff (the per-action ledger
	// skips completed actions); after repeated failures it escalates to
	// MigrationDirty.
	ReasonMigrationFailed = "MigrationFailed"

	// ReasonMigrationCoverageMissing: spec.version.requireMigrationCoverage is set
	// and a version transition crosses a major-version boundary with no migration
	// covering it. Fails closed rather than advancing a major change unmigrated.
	// Terminal until a covering migration is added or the version is corrected.
	ReasonMigrationCoverageMissing = "MigrationCoverageMissing"

	// ReasonMigrationDirty: a migration has failed repeatedly, so the controller
	// halts auto-retry rather than re-attempting destructive work against an
	// uncertain state. Sticky/terminal: cleared by a manual reconcile
	// (flux reconcile / reconcile.fluxcd.io/requestedAt) once the cause is fixed,
	// or by the transition completing. Mirrors golang-migrate dirty / Flyway repair.
	ReasonMigrationDirty = "MigrationDirty"

	// ReasonPreviousRevisionUnavailable: rollbackOnFailure could not restore
	// the last-good revisions because a producer has garbage-collected one.
	// Rollback is best-effort: it works only while producers retain.
	ReasonPreviousRevisionUnavailable = "PreviousRevisionUnavailable"

	// ReasonUpdateDeferred: a new revision (or initial deploy) is held by an
	// update window. Set on the Ready condition only when nothing is deployed
	// yet; an already-deployed StageSet stays Ready=True with status.pendingUpdate.
	ReasonUpdateDeferred = "UpdateDeferred"

	// ReasonStageFailed: a stage failed to fetch, build, apply, or verify. The
	// run halts at that stage; later stages do not run.
	ReasonStageFailed = "StageFailed"

	// ReasonRBACDenied: an apiserver call the reconciler made — resolving a
	// source CR, an impersonated tenant get/list, or the apply itself — failed
	// with Forbidden, or referenced a kind the apiserver does not know (the CRD
	// is not installed), or was rejected as schema-invalid. None recover by
	// retry: the cluster operator must grant the verb, install the CRD, or fix
	// the payload. Terminal — the reconciler stops engaging backoff so the
	// workqueue isn't burning cycles on a permanently-failing call. The message
	// names the call so kubectl describe sends operators straight to the fix.
	ReasonRBACDenied = "RBACDenied"

	// ReasonReady: all stages applied and verified at lastAppliedRevisions.
	ReasonReady = "Succeeded"
)

// AllReasons enumerates every wire-stable Reason the reconciler can set on the
// Ready condition. The drift-gate test in conditions_test.go asserts every
// entry has a matching docs/runbooks/<reason>.md and that the count matches the
// declared Reason* constants — so a new Reason cannot ship without a
// remediation page.
var AllReasons = []string{
	ReasonSuspended,
	ReasonInvalidSpec,
	ReasonSourceNotReady,
	ReasonArtifactNotFound,
	ReasonResolveFailed,
	ReasonDependencyNotReady,
	ReasonStalled,
	ReasonInvalidVersion,
	ReasonDowngradeRequiresMigration,
	ReasonMigrationStageNotFound,
	ReasonMigrationArtifactInvalid,
	ReasonMigrationSourceNotVerified,
	ReasonMigrationSourceNotPinned,
	ReasonMigrationCoverageMissing,
	ReasonMigrationFailed,
	ReasonMigrationDirty,
	ReasonPreviousRevisionUnavailable,
	ReasonUpdateDeferred,
	ReasonStageFailed,
	ReasonRBACDenied,
	ReasonReady,
}
