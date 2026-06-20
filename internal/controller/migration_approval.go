// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// approvedVersionAnnotation approves a held version transition: its value is the
// target version the operator authorizes. When spec.version.requireApproval is
// set and a transition has pending migrations, the controller proceeds only once
// this annotation equals the desired version (reason MigrationApprovalPending
// until then).
const approvedVersionAnnotation = "stages.metio.wtf/approved-version"

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
