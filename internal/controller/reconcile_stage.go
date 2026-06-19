// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

// reconcileStageAnnotation requests a single stage re-run its actions, even
// though its pinned revision is unchanged. The value is "<stage>@<token>": the
// stage name plus an opaque token compared against that stage's
// status.stages[].lastHandledReconcileAt so the request fires exactly once.
const reconcileStageAnnotation = "stages.metio.wtf/reconcile-stage"

// parseReconcileStage extracts the requested stage and token from the
// reconcile-stage annotation. An empty stage means no (or malformed) request.
func parseReconcileStage(ss *stagesv1.StageSet) (stage, token string) {
	v := ss.Annotations[reconcileStageAnnotation]
	if v == "" {
		return "", ""
	}
	name, tok, found := strings.Cut(v, "@")
	if !found || name == "" || tok == "" {
		return "", ""
	}
	return name, tok
}

// reconcileStageRequestedPredicate wakes the controller when the
// reconcile-stage annotation changes. The single-stage force-reconcile is a
// metadata-only edit — it bumps neither metadata.generation nor the Flux
// reconcile.fluxcd.io/requestedAt token, so neither GenerationChangedPredicate
// nor ReconcileRequestedPredicate would deliver the Update event. Scoped to the
// one key so an unrelated annotation edit still gets filtered out alongside the
// reconciler's own status writes.
type reconcileStageRequestedPredicate struct {
	predicate.Funcs
}

func (reconcileStageRequestedPredicate) Update(e event.UpdateEvent) bool {
	if e.ObjectOld == nil || e.ObjectNew == nil {
		return false
	}
	return e.ObjectOld.GetAnnotations()[reconcileStageAnnotation] !=
		e.ObjectNew.GetAnnotations()[reconcileStageAnnotation]
}
