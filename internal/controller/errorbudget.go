// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
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

// gateErrorBudget decides whether this run may proceed past the rollout-wide
// error-budget freeze. It returns deferred=true (with a requeue result) when the
// freeze holds a new-revision rollout; the caller then returns without applying.
// It is evaluated beside gateUpdateWindows and combined with it under AND.
//
// The freeze holds only NEW-revision rollouts (mirroring windowScope's Updates
// default): a frozen service still has its declared state enforced, so drift on
// the current revision keeps being corrected. With nothing new to roll out, the
// source is not even queried — the gate's only job is to decide a pending
// rollout.
func (r *StageSetReconciler) gateErrorBudget(ctx context.Context, helper *fluxpatch.Helper, ss *stagesv1.StageSet, resolved []artifact.ResolvedArtifact) (ctrl.Result, bool, error) {
	eb := ss.Spec.ErrorBudget
	if eb == nil {
		r.clearBudgetFreeze(ss)
		return ctrl.Result{}, false, nil
	}

	// Break-glass: ship once regardless of the budget.
	if token := ss.Annotations[budgetOverrideAnnotation]; token != "" && token != ss.Status.LastHandledBudgetOverride {
		ss.Status.LastHandledBudgetOverride = token
		r.clearBudgetFreeze(ss)
		return ctrl.Result{}, false, nil
	}

	freezeAt, resumeAt, perr := budgetThresholds(eb)
	if perr != nil {
		// A malformed threshold normally fails admission; this is the fallback.
		r.setReady(ss, metav1.ConditionFalse, ReasonInvalidSpec, fmt.Sprintf("spec.errorBudget: %v", perr))
		ss.Status.ObservedGeneration = ss.Generation
		return ctrl.Result{RequeueAfter: permanentRetryInterval}, true, r.patchStatus(ctx, helper, ss)
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

	interval := r.retryInterval(ss)
	if eb.Interval != nil && eb.Interval.Duration > 0 {
		interval = eb.Interval.Duration
	}

	value, qerr := r.MetricQuerier.Query(ctx, ss.Namespace, eb.Source)
	if qerr != nil {
		metrics.MetricSourceErrorsTotal.WithLabelValues(ss.Namespace, ss.Name).Inc()
		// onSourceError: Hold blocks the rollout; Allow (default) proceeds. Either
		// way the error is loud (metric above + event/condition here).
		if eb.OnSourceError == "Hold" {
			msg := fmt.Sprintf("error-budget source unavailable and onSourceError=Hold: %v", qerr)
			return r.deferBudget(ctx, helper, ss, ReasonBudgetSourceUnavailable, msg, interval)
		}
		// Allow: the rollout proceeds; surface the error without holding. The
		// continuing reconcile sets the final Ready condition, so only an event
		// is emitted here (deduped against the prior Ready state).
		prev := readyConditionSnapshot(ss)
		if prev == nil || prev.Reason != ReasonBudgetSourceUnavailable {
			r.event(ss, corev1.EventTypeWarning, ReasonBudgetSourceUnavailable,
				fmt.Sprintf("error-budget source unavailable; proceeding (onSourceError=Allow): %v", qerr))
		}
		r.clearBudgetFreeze(ss)
		return ctrl.Result{}, false, nil
	}

	metrics.BudgetRemaining.WithLabelValues(ss.Namespace, ss.Name).Set(value)

	// Hysteresis: once frozen, stay frozen until the budget recovers to
	// resumeThreshold; otherwise freeze when it drops below freezeThreshold.
	wasFrozen := ss.Status.BudgetFreeze != nil
	shouldFreeze := value < freezeAt
	if wasFrozen {
		shouldFreeze = value < resumeAt
	}

	if !shouldFreeze {
		r.clearBudgetFreeze(ss)
		return ctrl.Result{}, false, nil
	}

	// Record the freeze. Preserve the start instant across a steady freeze.
	since := metav1.Time{Time: r.now()}
	if wasFrozen && ss.Status.BudgetFreeze.Since != nil {
		since = *ss.Status.BudgetFreeze.Since
	}
	ss.Status.BudgetFreeze = &stagesv1.BudgetFreeze{
		Remaining:       strconv.FormatFloat(value, 'f', -1, 64),
		FreezeThreshold: eb.FreezeThreshold,
		ResumeThreshold: effectiveResume(eb),
		Since:           &since,
		DryRun:          eb.DryRun,
	}

	if eb.DryRun {
		// Record what would freeze; do not hold. The gauge stays 0 (not enforced).
		metrics.SetBudgetFrozen(ss.Namespace, ss.Name, false)
		if !wasFrozen {
			r.event(ss, corev1.EventTypeNormal, ReasonBudgetExhausted,
				fmt.Sprintf("error-budget freeze would activate (dryRun): remaining %s < freezeThreshold %s", ss.Status.BudgetFreeze.Remaining, eb.FreezeThreshold))
		}
		return ctrl.Result{}, false, nil
	}

	metrics.SetBudgetFrozen(ss.Namespace, ss.Name, true)
	msg := fmt.Sprintf("error budget exhausted: remaining %s < freezeThreshold %s; new-revision rollouts are frozen until it recovers to %s",
		ss.Status.BudgetFreeze.Remaining, eb.FreezeThreshold, effectiveResume(eb))
	return r.deferBudget(ctx, helper, ss, ReasonBudgetExhausted, msg, interval)
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
