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
	ReasonPreviousRevisionUnavailable,
	ReasonUpdateDeferred,
	ReasonStageFailed,
	ReasonReady,
}
