// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"errors"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

// promoteAnnotation manually promotes a stage past its promotion gate. The
// value is "<stage>@<token>": the stage name plus an opaque token compared
// against that stage's status.stages[].lastHandledPromotion so the promotion
// fires exactly once per new token. Mirrors reconcileStageAnnotation.
const promoteAnnotation = "stages.metio.wtf/promote"

// errHoldForPromotion is the stage-loop sentinel meaning "this stage is applied
// and healthy but its promotion gate is holding the rollout here." It is NOT a
// failure: no rollback, no backoff — the reconciler persists the stage statuses
// (so the soak clock and handled token survive), sets a held Ready condition,
// and requeues. Distinct from errTerminalStageFailure.
var errHoldForPromotion = errors.New("hold for promotion")

// parsePromote extracts the requested stage and token from the promote
// annotation. An empty stage means no (or malformed) request.
func parsePromote(ss *stagesv1.StageSet) (stage, token string) {
	v := ss.Annotations[promoteAnnotation]
	if v == "" {
		return "", ""
	}
	name, tok, found := strings.Cut(v, "@")
	if !found || name == "" || tok == "" {
		return "", ""
	}
	return name, tok
}

// promoteTokenFor returns the promote token addressed to stageName, or "" when
// the annotation is absent, malformed, or names a different stage.
func promoteTokenFor(ss *stagesv1.StageSet, stageName string) string {
	stage, token := parsePromote(ss)
	if stage != stageName {
		return ""
	}
	return token
}

// promoteRequestedPredicate wakes the controller when the promote annotation
// changes. A manual promotion is a metadata-only edit — it bumps neither
// metadata.generation nor the Flux reconcile.fluxcd.io/requestedAt token, so
// neither GenerationChangedPredicate nor ReconcileRequestedPredicate would
// deliver the Update event. Scoped to the one key so an unrelated annotation
// edit (and the reconciler's own status writes) stays filtered out.
type promoteRequestedPredicate struct {
	predicate.Funcs
}

func (promoteRequestedPredicate) Update(e event.UpdateEvent) bool {
	if e.ObjectOld == nil || e.ObjectNew == nil {
		return false
	}
	return e.ObjectOld.GetAnnotations()[promoteAnnotation] !=
		e.ObjectNew.GetAnnotations()[promoteAnnotation]
}

// gatePromotion decides whether a just-applied, healthy stage may advance the
// rollout. It evaluates the soak window first (stay healthy for the duration),
// then the manual gate (await an operator promote). It returns whether the
// stage is promoted, the promotion state to record on its status, how long to
// requeue while holding, and the promote token to record as handled.
//
// State is keyed to the stage's applied revision: a new revision restarts the
// soak from scratch, and a stage already Promoted at this revision short-
// circuits so a drift reconcile never re-soaks it. The hold is status-only and
// re-evaluated from scratch each reconcile (see the design's self-review): if
// status is lost the soak restarts, which is acceptable.
func (r *StageSetReconciler) gatePromotion(ss *stagesv1.StageSet, stage *stagesv1.Stage, revision string, prior stagesv1.StageStatus, now time.Time) (promoted bool, state *stagesv1.PromotionState, requeueAfter time.Duration, handled string) {
	p := stage.Promotion
	if p == nil {
		return true, nil, 0, prior.LastHandledPromotion
	}

	sameRevision := prior.AppliedRevision == revision

	// Already cleared the gate for this revision — don't re-soak a promoted
	// stage on every drift-correction reconcile.
	if sameRevision && prior.PromotionState != nil && prior.PromotionState.Phase == stagesv1.PromotionPromoted {
		return true, prior.PromotionState, 0, prior.LastHandledPromotion
	}

	// Manual promote (break-glass): a fresh token addressed to this stage
	// advances it immediately, ending a soak early or satisfying a manual gate.
	// Checked before the soak so `stagesetctl promote` is a universal "advance
	// this stage now," whatever is currently holding it.
	if tok := promoteTokenFor(ss, stage.Name); tok != "" && tok != prior.LastHandledPromotion {
		return true, &stagesv1.PromotionState{Phase: stagesv1.PromotionPromoted, Since: &metav1.Time{Time: now}}, 0, tok
	}

	// (a) Soak — hold until the stage has stayed healthy for the whole window.
	if p.Soak != nil && p.Soak.Duration > 0 {
		since := now
		if sameRevision && prior.PromotionState != nil && prior.PromotionState.Since != nil {
			since = prior.PromotionState.Since.Time
		}
		soakUntil := since.Add(p.Soak.Duration)
		if now.Before(soakUntil) {
			state = &stagesv1.PromotionState{
				Phase:     stagesv1.PromotionSoaking,
				Since:     &metav1.Time{Time: since},
				SoakUntil: &metav1.Time{Time: soakUntil},
			}
			return false, state, requeueForWindow(soakUntil, now, r.retryInterval(ss)), prior.LastHandledPromotion
		}
	}

	// (b) Manual gate — hold until an operator promotes the stage. A fresh
	// promote token was already consumed above, so reaching here means none is
	// pending.
	if p.RequireManualPromotion {
		since := now
		if sameRevision && prior.PromotionState != nil &&
			prior.PromotionState.Phase == stagesv1.PromotionAwaitingManual && prior.PromotionState.Since != nil {
			since = prior.PromotionState.Since.Time
		}
		state = &stagesv1.PromotionState{Phase: stagesv1.PromotionAwaitingManual, Since: &metav1.Time{Time: since}}
		return false, state, r.retryInterval(ss), prior.LastHandledPromotion
	}

	// Soak elapsed (or none) and no manual gate — promote.
	return true, &stagesv1.PromotionState{Phase: stagesv1.PromotionPromoted, Since: &metav1.Time{Time: now}}, 0, prior.LastHandledPromotion
}
