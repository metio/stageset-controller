// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"github.com/Masterminds/semver/v3"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

// approvedVersionAnnotation approves a held version transition: its value is the
// target version the operator (or a FleetRollout) authorizes. When
// spec.version.approvalMode holds a transition, the controller proceeds only once
// this annotation equals the desired version (reason AwaitingApproval until then).
const approvedVersionAnnotation = "stages.metio.wtf/approved-version"

// rollbackToAnnotation directs a downgrade: when its value is a version below the
// deployed status.version, it overrides the source-resolved desired version, so
// the controller rolls the StageSet back to it via the downgrade path (subject to
// spec.version.allowDowngrade and the crossed migrations' down actions). A
// FleetRollout stamps it on regression when onRegression is Rollback; an operator
// can stamp it by hand for a manual rollback. It self-clears in effect: once the
// version reaches the target it is no longer below current, so it stops applying.
const rollbackToAnnotation = "stages.metio.wtf/rollback-to"

// rollbackDirective returns the version a rollback-to annotation directs the
// StageSet down to, or "" when there is none in effect — the annotation is unset,
// unparseable, or not below the deployed version. Overriding the source-resolved
// desired version with a lower one is what engages the downgrade path.
func rollbackDirective(ss *stagesv1.StageSet) string {
	rb := ss.Annotations[rollbackToAnnotation]
	if rb == "" || ss.Status.Version == "" {
		return ""
	}
	rbV, err := semver.NewVersion(rb)
	if err != nil {
		return ""
	}
	curV, err := semver.NewVersion(ss.Status.Version)
	if err != nil {
		return ""
	}
	if rbV.LessThan(curV) {
		return rb
	}
	return ""
}

// versionApprovalNeeded reports whether a version transition must be approved
// before it proceeds, per spec.version.approvalMode. Baselining (first adoption)
// and a no-op reconcile are never held; OnMigrations holds only a transition that
// carries migrations; Always holds every real version change (up or down).
func versionApprovalNeeded(mode stagesv1.ApprovalMode, plan *migrationPlan, currentVersion string) bool {
	if plan == nil || !plan.versionSet || plan.baseline {
		return false
	}
	switch mode {
	case stagesv1.ApprovalAlways:
		return plan.desired != currentVersion
	case stagesv1.ApprovalOnMigrations:
		return len(plan.pending) > 0
	default: // Never / unset
		return false
	}
}

// migrationApprovalPredicate wakes the controller when the approved-version
// annotation changes. Approving is a metadata-only edit — it bumps neither
// metadata.generation nor the Flux reconcile.fluxcd.io/requestedAt token, so
// neither GenerationChangedPredicate nor ReconcileRequestedPredicate would
// deliver the Update event. Scoped to the one key so an unrelated annotation
// edit (and the reconciler's own status writes) stays filtered out.
type migrationApprovalPredicate struct {
	predicate.Funcs
}

func (migrationApprovalPredicate) Update(e event.UpdateEvent) bool {
	if e.ObjectOld == nil || e.ObjectNew == nil {
		return false
	}
	old, cur := e.ObjectOld.GetAnnotations(), e.ObjectNew.GetAnnotations()
	return old[approvedVersionAnnotation] != cur[approvedVersionAnnotation] ||
		old[rollbackToAnnotation] != cur[rollbackToAnnotation]
}
