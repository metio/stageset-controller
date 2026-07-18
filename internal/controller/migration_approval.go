// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

// approvedVersionAnnotation approves a held version transition: its value is the
// target version the operator (or a FleetRollout) authorizes. When
// spec.version.approvalMode holds a transition, the controller proceeds only once
// this annotation equals the desired version (reason AwaitingApproval until then).
const approvedVersionAnnotation = "stages.metio.wtf/approved-version"

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
	return e.ObjectOld.GetAnnotations()[approvedVersionAnnotation] !=
		e.ObjectNew.GetAnnotations()[approvedVersionAnnotation]
}
