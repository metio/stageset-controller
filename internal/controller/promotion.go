// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"errors"
	"fmt"
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

// errGateUnevaluable is the stage-loop sentinel for a promotion gate that could
// not be EVALUATED this reconcile — a transient RBAC/apiserver error reading the
// watched pods (restart gate) or events (event gate), or a transient failure
// applying an onFailure=Rollback revert. The stage itself applied successfully,
// so this is NOT a stage failure: the reconciler backs off and retries without
// rolling the healthy rollout back. Distinct from a failStage error, which is
// the only thing that may engage rollbackOnFailure.
var errGateUnevaluable = errors.New("promotion gate unevaluable")

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
// rollout. It evaluates, in order: a manual break-glass promote, an analysis
// hard-failure (onFailure), the soak window, an in-progress analysis (must
// currently pass after the soak), and finally the manual gate. It returns
// whether the stage is promoted, the promotion state to record on its status,
// how long to requeue while holding, the promote token to record as handled, and
// whether an analysis failure with onFailure=Rollback wants the stage reverted.
//
// State is keyed to the stage's applied revision: a new revision restarts the
// soak and the analysis failure count from scratch, and a stage already Promoted
// at this revision short-circuits so a drift reconcile never re-gates it. The
// hold is status-only and re-evaluated from scratch each reconcile (see the
// design's self-review): if status is lost the soak restarts, which is
// acceptable. verdict carries the metric-analysis evaluation for this reconcile,
// or nil when the stage has no analysis.
func (r *StageSetReconciler) gatePromotion(ss *stagesv1.StageSet, stage *stagesv1.Stage, revision string, prior stagesv1.StageStatus, now time.Time, verdict *analysisVerdict, fastTrackOK bool, restart *restartVerdict, event *eventVerdict) (promoted bool, state *stagesv1.PromotionState, requeueAfter time.Duration, handled string, rollback bool) {
	p := stage.Promotion
	if p == nil {
		return true, nil, 0, prior.LastHandledPromotion, false
	}

	sameRevision := prior.AppliedRevision == revision
	priorState := prior.PromotionState

	// Already cleared the gate for this revision — don't re-gate a promoted stage
	// on every drift-correction reconcile.
	if sameRevision && priorState != nil && priorState.Phase == stagesv1.PromotionPromoted {
		return true, priorState, 0, prior.LastHandledPromotion, false
	}

	// Manual promote (break-glass): a fresh token addressed to this stage
	// advances it immediately, ending a soak early, satisfying a manual gate, or
	// overriding a blocked analysis. Checked first so `stagesetctl promote` is a
	// universal "advance this stage now," whatever is currently holding it.
	if tok := promoteTokenFor(ss, stage.Name); tok != "" && tok != prior.LastHandledPromotion {
		return true, &stagesv1.PromotionState{Phase: stagesv1.PromotionPromoted, Since: &metav1.Time{Time: now}}, 0, tok, false
	}

	// Restart gate: a watched pod group has restarted more than its check allows.
	// Block the advance — a crashlooping stage must not promote even if its
	// workload still reports Ready. The manual promote above overrides this, and
	// drift on the current stage keeps being corrected.
	if restart != nil {
		s := carrySoakUntil(&stagesv1.PromotionState{
			Phase:            stagesv1.PromotionBlocked,
			Since:            phaseSince(priorState, sameRevision, stagesv1.PromotionBlocked, now),
			RestartCheck:     restart.check,
			ObservedRestarts: restart.observed,
		}, priorState, sameRevision)
		return false, s, r.retryInterval(ss), prior.LastHandledPromotion, restart.rollback
	}

	// Event gate: a watched pod group has accumulated too many Warning events.
	// Block the advance (or roll back) — same semantics as the restart gate.
	if event != nil {
		s := carrySoakUntil(&stagesv1.PromotionState{
			Phase:          stagesv1.PromotionBlocked,
			Since:          phaseSince(priorState, sameRevision, stagesv1.PromotionBlocked, now),
			EventCheck:     event.check,
			ObservedEvents: event.observed,
		}, priorState, sameRevision)
		return false, s, r.retryInterval(ss), prior.LastHandledPromotion, event.rollback
	}

	// Analysis bookkeeping: count consecutive failing evaluations for this
	// revision; a failure is a threshold breach, or an unreadable source unless
	// onSourceError=Allow.
	var (
		result   *stagesv1.AnalysisResult
		failures int32
		passing  = true
		failed   bool
	)
	if verdict != nil {
		an := p.Analysis
		result = verdict.result
		failingNow := verdict.breached || (verdict.sourceErr && an.OnSourceError != "Allow")
		passing = !failingNow
		prevFailures := int32(0)
		if sameRevision && priorState != nil {
			prevFailures = priorState.AnalysisFailures
		}
		if failingNow {
			failures = prevFailures + 1
		}
		limit := int32(0)
		if an.FailureLimit != nil {
			limit = *an.FailureLimit
		}
		if failures > limit && !an.DryRun {
			failed = true
		}
	}
	withAnalysis := func(s *stagesv1.PromotionState) *stagesv1.PromotionState {
		s.AnalysisFailures = failures
		s.LastAnalysis = result
		return s
	}

	// Analysis hard-failure → onFailure (Hold leaves the stage applied-but-held;
	// Rollback signals the caller to revert this stage).
	if failed {
		s := withAnalysis(carrySoakUntil(&stagesv1.PromotionState{Phase: stagesv1.PromotionBlocked, Since: phaseSince(priorState, sameRevision, stagesv1.PromotionBlocked, now)}, priorState, sameRevision))
		return false, s, r.analysisInterval(ss, p.Analysis), prior.LastHandledPromotion, p.Analysis.OnFailure == "Rollback"
	}

	// Soak — hold until the stage has stayed healthy for the whole window. Skip
	// once a post-soak phase has been reached at this revision so a later hold
	// (analysis/manual) does not restart the soak clock.
	if p.Soak != nil && p.Soak.Duration > 0 && !(sameRevision && priorState != nil && pastSoak(priorState.Phase)) {
		since := now
		switch {
		case sameRevision && priorState != nil && priorState.SoakUntil != nil:
			// Resume the deadline anchored when this revision first entered the
			// soak, even across an intervening Blocked hold, so a transient block
			// during the bake neither skips nor restarts the soak.
			since = priorState.SoakUntil.Time.Add(-p.Soak.Duration)
		case sameRevision && priorState != nil && priorState.Phase == stagesv1.PromotionSoaking && priorState.Since != nil:
			since = priorState.Since.Time
		}
		soakUntil := since.Add(p.Soak.Duration)
		// Fast-track: once the minimum soak has elapsed and the burn-rate gate is
		// healthy, skip the rest of the soak and fall through to the remaining
		// gates (analysis/manual). Never extends the soak — only shortens it.
		fastTracked := p.FastTrack != nil && fastTrackOK && !now.Before(since.Add(fastTrackAfter(p.FastTrack)))
		if now.Before(soakUntil) && !fastTracked {
			s := withAnalysis(&stagesv1.PromotionState{
				Phase:     stagesv1.PromotionSoaking,
				Since:     &metav1.Time{Time: since},
				SoakUntil: &metav1.Time{Time: soakUntil},
			})
			return false, s, requeueForWindow(soakUntil, now, r.retryInterval(ss)), prior.LastHandledPromotion, false
		}
	}

	// After the soak: a present analysis must currently pass before advancing —
	// never promote on a momentarily-failing check still within failureLimit.
	if verdict != nil && !passing && !p.Analysis.DryRun {
		s := withAnalysis(&stagesv1.PromotionState{Phase: stagesv1.PromotionAnalyzing, Since: phaseSince(priorState, sameRevision, stagesv1.PromotionAnalyzing, now)})
		return false, s, r.analysisInterval(ss, p.Analysis), prior.LastHandledPromotion, false
	}

	// Manual gate — hold until an operator promotes the stage. A fresh promote
	// token was already consumed above, so reaching here means none is pending.
	if p.RequireManualPromotion {
		since := now
		if sameRevision && priorState != nil &&
			priorState.Phase == stagesv1.PromotionAwaitingManual && priorState.Since != nil {
			since = priorState.Since.Time
		}
		s := withAnalysis(&stagesv1.PromotionState{Phase: stagesv1.PromotionAwaitingManual, Since: &metav1.Time{Time: since}})
		return false, s, r.retryInterval(ss), prior.LastHandledPromotion, false
	}

	// Soak elapsed (or none), analysis passing (or none), no manual gate —
	// promote.
	return true, withAnalysis(&stagesv1.PromotionState{Phase: stagesv1.PromotionPromoted, Since: &metav1.Time{Time: now}}), 0, prior.LastHandledPromotion, false
}

// promotionHoldCondition maps a held promotion phase to the Ready reason and
// message surfaced on the StageSet. Soaking and an in-progress analysis are
// healthy holds (ReasonSoaking); a failed analysis is ReasonPromotionBlocked; a
// manual gate is ReasonAwaitingPromotion.
func promotionHoldCondition(ss *stagesv1.StageSet, stage string, state *stagesv1.PromotionState) (reason, msg string) {
	phase := stagesv1.PromotionSoaking
	if state != nil {
		phase = state.Phase
	}
	switch phase {
	case stagesv1.PromotionAwaitingManual:
		return ReasonAwaitingPromotion, fmt.Sprintf(
			"stage %q is healthy and awaiting a manual promotion; run: stagesetctl promote %s --namespace %s --stage %s",
			stage, ss.Name, ss.Namespace, stage,
		)
	case stagesv1.PromotionBlocked:
		if state != nil && state.RestartCheck != "" {
			if state.AbortedRevision != "" {
				return ReasonPromotionBlocked, fmt.Sprintf(
					"stage %q restart check %q observed %d pod restart(s) over its limit; rolled back to its last-good revision",
					stage, state.RestartCheck, state.ObservedRestarts,
				)
			}
			return ReasonPromotionBlocked, fmt.Sprintf(
				"stage %q restart check %q observed %d pod restart(s) over its limit; the rollout will not advance past it",
				stage, state.RestartCheck, state.ObservedRestarts,
			)
		}
		if state != nil && state.EventCheck != "" {
			if state.AbortedRevision != "" {
				return ReasonPromotionBlocked, fmt.Sprintf(
					"stage %q event check %q observed %d warning event(s) over its limit; rolled back to its last-good revision",
					stage, state.EventCheck, state.ObservedEvents,
				)
			}
			return ReasonPromotionBlocked, fmt.Sprintf(
				"stage %q event check %q observed %d warning event(s) over its limit; the rollout will not advance past it",
				stage, state.EventCheck, state.ObservedEvents,
			)
		}
		if state != nil && state.AbortedRevision != "" {
			return ReasonPromotionBlocked, fmt.Sprintf(
				"stage %q promotion analysis failed (%d failure(s)); rolled back to its last-good revision",
				stage, state.AnalysisFailures,
			)
		}
		return ReasonPromotionBlocked, fmt.Sprintf(
			"stage %q promotion analysis is failing (%d failure(s)); the rollout will not advance past it",
			stage, analysisFailureCount(state),
		)
	case stagesv1.PromotionAnalyzing:
		return ReasonSoaking, fmt.Sprintf(
			"stage %q is running promotion analysis before advancing", stage,
		)
	default: // Soaking
		if state != nil && state.SoakUntil != nil {
			return ReasonSoaking, fmt.Sprintf(
				"stage %q is soaking before promotion; soak window closes at %s",
				stage, state.SoakUntil.Time.UTC().Format(time.RFC3339),
			)
		}
		return ReasonSoaking, fmt.Sprintf("stage %q is soaking before promotion", stage)
	}
}

func analysisFailureCount(state *stagesv1.PromotionState) int32 {
	if state == nil {
		return 0
	}
	return state.AnalysisFailures
}

// pastSoak reports whether a promotion phase is one the gate only reaches AFTER
// the soak window has elapsed, so the soak clock isn't restarted on a later hold.
// PromotionBlocked is deliberately excluded: the restart gate, event gate, and
// an analysis hard-failure all set Blocked BEFORE the soak block runs, so a
// Blocked phase can be reached mid-soak. Treating it as past-soak would let a
// transient block that clears skip the remaining soak and promote early. The
// soak deadline carried on the Blocked state (see carrySoakUntil) instead lets
// the soak resume its original window.
func pastSoak(phase stagesv1.PromotionPhase) bool {
	switch phase {
	case stagesv1.PromotionAnalyzing, stagesv1.PromotionAwaitingManual, stagesv1.PromotionPromoted:
		return true
	default:
		return false
	}
}

// carrySoakUntil preserves a running soak's deadline onto a hold reached during
// the bake (a restart/event/analysis breach mid-soak), so when the hold clears
// the soak resumes its ORIGINAL deadline — it neither skips the remaining soak
// (Blocked is not treated as past-soak) nor restarts it from scratch.
func carrySoakUntil(s, priorState *stagesv1.PromotionState, sameRevision bool) *stagesv1.PromotionState {
	if sameRevision && priorState != nil && priorState.SoakUntil != nil {
		s.SoakUntil = priorState.SoakUntil
	}
	return s
}

// phaseSince preserves a phase's start instant across reconciles that stay in
// the same phase at the same revision, and stamps now otherwise.
func phaseSince(priorState *stagesv1.PromotionState, sameRevision bool, phase stagesv1.PromotionPhase, now time.Time) *metav1.Time {
	if sameRevision && priorState != nil && priorState.Phase == phase && priorState.Since != nil {
		return priorState.Since
	}
	return &metav1.Time{Time: now}
}
