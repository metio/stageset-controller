// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	fluxpatch "github.com/fluxcd/pkg/runtime/patch"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/artifact"
	"github.com/metio/stageset-controller/internal/metrics"
	"github.com/metio/stageset-controller/internal/metricsource"
)

// budgetOverrideAnnotation forces a held rollout through an error-budget freeze
// once, regardless of the source's value (tracked in
// status.lastHandledBudgetOverride). The break-glass for shipping a reliability
// fix while out of budget — the SRE policy explicitly exempts such changes.
const budgetOverrideAnnotation = "stages.metio.wtf/budget-override"

// readSecretData reads a Secret in a namespace through the controller's client,
// for the metric-source bearer token. Returns the data map.
func (r *StageSetReconciler) readSecretData(ctx context.Context, namespace, name string) (map[string][]byte, error) {
	var sec corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &sec); err != nil {
		return nil, err
	}
	return sec.Data, nil
}

// errHoldForBudget is the stage-loop sentinel meaning "a new revision is frozen
// from entering this stage by the stage's own errorBudget." Like
// errHoldForPromotion it is a hold, not a failure: earlier stages stay applied,
// this stage keeps its current revision, later stages are not processed.
var errHoldForBudget = errors.New("hold for stage error budget")

// freshBudgetOverride reports the budget-override token when it is present and
// not yet handled — the one-shot break-glass that ships a rollout once regardless
// of any budget (rollout-wide or per-stage).
func freshBudgetOverride(ss *stagesv1.StageSet) (token string, fresh bool) {
	t := ss.Annotations[budgetOverrideAnnotation]
	return t, t != "" && t != ss.Status.LastHandledBudgetOverride
}

// budgetVerdict is the outcome of evaluating one errorBudget against its source,
// independent of scope (rollout-wide or per-stage). The caller wires status,
// conditions, and scoped metrics.
type budgetVerdict struct {
	value     float64
	freeze    *stagesv1.BudgetFreeze // the freeze state to record; nil when not frozen
	frozen    bool                   // hysteresis decision (before the dryRun gate)
	sourceErr error                  // source unreadable
	specErr   error                  // malformed thresholds (terminal)
}

// evaluateBudget queries eb's source and decides, with hysteresis against
// prevFreeze, whether the budget is frozen. It carries no status/condition/metric
// side effects so both the rollout-wide and per-stage gates can share it.
func (r *StageSetReconciler) evaluateBudget(ctx context.Context, namespace string, eb *stagesv1.ErrorBudget, prevFreeze *stagesv1.BudgetFreeze) budgetVerdict {
	freezeAt, resumeAt, perr := budgetThresholds(eb)
	if perr != nil {
		return budgetVerdict{specErr: perr}
	}
	value, qerr := r.MetricQuerier.Query(ctx, namespace, eb.Source)
	if qerr != nil {
		return budgetVerdict{sourceErr: qerr}
	}
	// Hysteresis: once frozen, stay frozen until the budget recovers to
	// resumeThreshold; otherwise freeze when it drops below freezeThreshold.
	wasFrozen := prevFreeze != nil
	frozen := value < freezeAt
	if wasFrozen {
		frozen = value < resumeAt
	}
	if !frozen {
		return budgetVerdict{value: value}
	}
	since := metav1.Time{Time: r.now()}
	if wasFrozen && prevFreeze.Since != nil {
		since = *prevFreeze.Since
	}
	return budgetVerdict{
		value:  value,
		frozen: true,
		freeze: &stagesv1.BudgetFreeze{
			Remaining:       strconv.FormatFloat(value, 'f', -1, 64),
			FreezeThreshold: eb.FreezeThreshold,
			ResumeThreshold: effectiveResume(eb),
			Since:           &since,
			DryRun:          eb.DryRun,
		},
	}
}

// budgetInterval is the re-check cadence while a budget freeze holds.
func (r *StageSetReconciler) budgetInterval(ss *stagesv1.StageSet, eb *stagesv1.ErrorBudget) time.Duration {
	if eb.Interval != nil && eb.Interval.Duration > 0 {
		return eb.Interval.Duration
	}
	return r.retryInterval(ss)
}

// gateErrorBudget decides whether this run may proceed past the rollout-wide
// error-budget freeze. It returns deferred=true (with a requeue result) when the
// freeze holds a new-revision rollout; the caller then returns without applying.
// It is evaluated beside gateUpdateWindows and combined with it under AND.
//
// The freeze holds only NEW-revision rollouts (mirroring windowScope's Updates
// default): a frozen service still has its declared state enforced, so drift on
// the current revision keeps being corrected. With nothing new to roll out, the
// source is not even queried — the gate's only job is to decide a pending
// rollout. override short-circuits the freeze (the break-glass, consumed by the
// caller).
func (r *StageSetReconciler) gateErrorBudget(ctx context.Context, helper *fluxpatch.Helper, ss *stagesv1.StageSet, resolved []artifact.ResolvedArtifact, override bool) (ctrl.Result, bool, error) {
	eb := ss.Spec.ErrorBudget
	if eb == nil || override {
		r.clearBudgetFreeze(ss)
		return ctrl.Result{}, false, nil
	}

	// Only a new revision is gated; a steady reconcile (drift correction) flows.
	newRevision := false
	for _, ra := range resolved {
		if ss.Status.LastAppliedRevisions[ra.Key()] != ra.Revision {
			newRevision = true
			break
		}
	}
	if !newRevision {
		r.clearBudgetFreeze(ss)
		return ctrl.Result{}, false, nil
	}

	interval := r.budgetInterval(ss, eb)
	v := r.evaluateBudget(ctx, ss.Namespace, eb, ss.Status.BudgetFreeze)
	switch {
	case v.specErr != nil:
		// A malformed threshold normally fails admission; this is the fallback.
		r.setReady(ss, metav1.ConditionFalse, ReasonInvalidSpec, fmt.Sprintf("spec.errorBudget: %v", v.specErr))
		ss.Status.ObservedGeneration = ss.Generation
		return ctrl.Result{RequeueAfter: permanentRetryInterval}, true, r.patchStatus(ctx, helper, ss)
	case v.sourceErr != nil:
		metrics.MetricSourceErrorsTotal.WithLabelValues(ss.Namespace, ss.Name).Inc()
		if eb.OnSourceError == "Hold" {
			msg := fmt.Sprintf("error-budget source unavailable and onSourceError=Hold: %v", v.sourceErr)
			return r.deferBudget(ctx, helper, ss, ReasonBudgetSourceUnavailable, msg, interval)
		}
		// Allow (default): proceed; surface the error without holding. The
		// continuing reconcile sets the final Ready condition, so only an event
		// is emitted here (deduped against the prior Ready state).
		prev := readyConditionSnapshot(ss)
		if prev == nil || prev.Reason != ReasonBudgetSourceUnavailable {
			r.event(ss, corev1.EventTypeWarning, ReasonBudgetSourceUnavailable,
				fmt.Sprintf("error-budget source unavailable; proceeding (onSourceError=Allow): %v", v.sourceErr))
		}
		r.clearBudgetFreeze(ss)
		return ctrl.Result{}, false, nil
	}

	metrics.BudgetRemaining.WithLabelValues(ss.Namespace, ss.Name).Set(v.value)
	if !v.frozen {
		r.clearBudgetFreeze(ss)
		return ctrl.Result{}, false, nil
	}
	ss.Status.BudgetFreeze = v.freeze

	if eb.DryRun {
		metrics.SetBudgetFrozen(ss.Namespace, ss.Name, false)
		if prev := readyConditionSnapshot(ss); prev == nil || prev.Reason != ReasonBudgetExhausted {
			r.event(ss, corev1.EventTypeNormal, ReasonBudgetExhausted,
				fmt.Sprintf("error-budget freeze would activate (dryRun): remaining %s < freezeThreshold %s", v.freeze.Remaining, eb.FreezeThreshold))
		}
		return ctrl.Result{}, false, nil
	}

	metrics.SetBudgetFrozen(ss.Namespace, ss.Name, true)
	msg := fmt.Sprintf("error budget exhausted: remaining %s < freezeThreshold %s; new-revision rollouts are frozen until it recovers to %s",
		v.freeze.Remaining, eb.FreezeThreshold, effectiveResume(eb))
	return r.deferBudget(ctx, helper, ss, ReasonBudgetExhausted, msg, interval)
}

// stageBudgetGate is how a per-stage errorBudget resolves for one reconcile.
type stageBudgetGate struct {
	hold      bool                   // the rollout must hold at this stage
	freeze    *stagesv1.BudgetFreeze // freeze to record on the stage status (nil = none)
	reason    string                 // Ready reason when holding
	msg       string
	requeue   time.Duration
	specErr   error // malformed thresholds (terminal)
	sourceErr error // source unreadable under onSourceError=Allow (proceed, but event it)
}

// gateStageBudget evaluates a stage's own errorBudget for a new-revision entry.
// It mirrors gateErrorBudget but scoped to one stage, writing the per-stage
// frozen gauge; the caller wires the held status/condition and the dryRun freeze
// record. Only called when the stage has a new revision and no override is active.
func (r *StageSetReconciler) gateStageBudget(ctx context.Context, ss *stagesv1.StageSet, stage *stagesv1.Stage, prior stagesv1.StageStatus) stageBudgetGate {
	eb := stage.ErrorBudget
	interval := r.budgetInterval(ss, eb)
	v := r.evaluateBudget(ctx, ss.Namespace, eb, prior.BudgetFreeze)
	switch {
	case v.specErr != nil:
		return stageBudgetGate{specErr: v.specErr}
	case v.sourceErr != nil:
		metrics.MetricSourceErrorsTotal.WithLabelValues(ss.Namespace, ss.Name).Inc()
		if eb.OnSourceError == "Hold" {
			return stageBudgetGate{
				hold:    true,
				reason:  ReasonBudgetSourceUnavailable,
				msg:     fmt.Sprintf("stage %q error-budget source unavailable and onSourceError=Hold: %v", stage.Name, v.sourceErr),
				requeue: interval,
			}
		}
		// Allow: proceed. The stage is not held, so drop any freeze gauge a prior
		// exhausted-budget reconcile left set — the gauge tracks an active hold,
		// which this reconcile no longer has.
		metrics.SetStageBudgetFrozen(ss.Namespace, ss.Name, stage.Name, false)
		return stageBudgetGate{sourceErr: v.sourceErr}
	}
	if !v.frozen {
		metrics.SetStageBudgetFrozen(ss.Namespace, ss.Name, stage.Name, false)
		return stageBudgetGate{}
	}
	if eb.DryRun {
		metrics.SetStageBudgetFrozen(ss.Namespace, ss.Name, stage.Name, false)
		return stageBudgetGate{freeze: v.freeze} // record what would freeze; do not hold
	}
	metrics.SetStageBudgetFrozen(ss.Namespace, ss.Name, stage.Name, true)
	return stageBudgetGate{
		hold:   true,
		freeze: v.freeze,
		reason: ReasonBudgetExhausted,
		msg: fmt.Sprintf("stage %q error budget exhausted: remaining %s < freezeThreshold %s; the new revision is held from entering this stage until it recovers to %s",
			stage.Name, v.freeze.Remaining, eb.FreezeThreshold, effectiveResume(eb)),
		requeue: interval,
	}
}

// deferBudget holds the rollout for a budget reason: an already-deployed StageSet
// stays Ready=True (its current state is healthy — this is a deliberate wait),
// while an undeployed one reports Ready=False with the reason. The event is
// deduped against the prior Ready state, and the rollout requeues at interval to
// re-check the budget.
func (r *StageSetReconciler) deferBudget(ctx context.Context, helper *fluxpatch.Helper, ss *stagesv1.StageSet, reason, msg string, interval time.Duration) (ctrl.Result, bool, error) {
	prev := readyConditionSnapshot(ss)
	if len(ss.Status.LastAppliedRevisions) > 0 {
		r.setReady(ss, metav1.ConditionTrue, ReasonReady, "Deployed; "+msg)
		r.emitReadyEvent(ss, prev, metav1.ConditionTrue, ReasonReady, msg)
	} else {
		r.setReady(ss, metav1.ConditionFalse, reason, msg)
		r.emitReadyEvent(ss, prev, metav1.ConditionFalse, reason, msg)
	}
	ss.Status.ObservedGeneration = ss.Generation
	if uerr := r.patchStatus(ctx, helper, ss); uerr != nil {
		return ctrl.Result{}, true, uerr
	}
	return ctrl.Result{RequeueAfter: interval}, true, nil
}

// clearBudgetFreeze drops any recorded freeze and zeroes the frozen gauge.
func (r *StageSetReconciler) clearBudgetFreeze(ss *stagesv1.StageSet) {
	ss.Status.BudgetFreeze = nil
	metrics.SetBudgetFrozen(ss.Namespace, ss.Name, false)
}

// budgetThresholds parses freeze and resume thresholds, defaulting resume to
// freeze (no hysteresis) when unset.
func budgetThresholds(eb *stagesv1.ErrorBudget) (freeze, resume float64, err error) {
	freeze, err = metricsource.ParseScalar(eb.FreezeThreshold)
	if err != nil {
		return 0, 0, fmt.Errorf("freezeThreshold %q is not a number: %w", eb.FreezeThreshold, err)
	}
	if eb.ResumeThreshold == "" {
		return freeze, freeze, nil
	}
	resume, err = metricsource.ParseScalar(eb.ResumeThreshold)
	if err != nil {
		return 0, 0, fmt.Errorf("resumeThreshold %q is not a number: %w", eb.ResumeThreshold, err)
	}
	if resume < freeze {
		return 0, 0, fmt.Errorf("resumeThreshold %q must be >= freezeThreshold %q (hysteresis resumes at or above the freeze point)", eb.ResumeThreshold, eb.FreezeThreshold)
	}
	return freeze, resume, nil
}

// effectiveResume is the effective resume threshold string for status/messages.
func effectiveResume(eb *stagesv1.ErrorBudget) string {
	if eb.ResumeThreshold != "" {
		return eb.ResumeThreshold
	}
	return eb.FreezeThreshold
}
