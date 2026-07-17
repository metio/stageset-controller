// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"errors"
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

// errNoLifetimeLedger guards the unreachable nil-ledger path in
// recordLifetimeCompletion; a Lifetime action always materializes its ledger.
var errNoLifetimeLedger = errors.New("scope: Lifetime action has no StageLedger to record into")

// specHasLifetimeActions reports whether any stage carries a scope: Lifetime
// pre/post action — the trigger to materialize a StageLedger.
func specHasLifetimeActions(ss *stagesv1.StageSet) bool {
	for i := range ss.Spec.Stages {
		if len(stageLifetimeActionNames(&ss.Spec.Stages[i])) > 0 {
			return true
		}
	}
	return false
}

// stageLifetimeActionNames lists a stage's pre+post action names scoped Lifetime.
func stageLifetimeActionNames(stage *stagesv1.Stage) []string {
	if stage.Actions == nil {
		return nil
	}
	var out []string
	for i := range stage.Actions.Pre {
		if stage.Actions.Pre[i].EffectiveScope() == stagesv1.ScopeLifetime {
			out = append(out, stage.Actions.Pre[i].Name)
		}
	}
	for i := range stage.Actions.Post {
		if stage.Actions.Post[i].EffectiveScope() == stagesv1.ScopeLifetime {
			out = append(out, stage.Actions.Post[i].Name)
		}
	}
	return out
}

// isLifetimeAction reports whether (stage, action) names a scope: Lifetime
// pre/post action in the current spec — used to decide whether a spec.baseline
// entry resolves to something real before promoting it.
func isLifetimeAction(ss *stagesv1.StageSet, stage, action string) bool {
	for i := range ss.Spec.Stages {
		st := &ss.Spec.Stages[i]
		if st.Name != stage {
			continue
		}
		if slices.Contains(stageLifetimeActionNames(st), action) {
			return true
		}
	}
	return false
}

// lifetimeDoneForStage returns the Lifetime action names the ledger records
// complete for one stage — the gate set for that stage.
func lifetimeDoneForStage(ledger *stagesv1.StageLedger, stage string) []string {
	if ledger == nil {
		return nil
	}
	var out []string
	for i := range ledger.Status.CompletedActions {
		c := &ledger.Status.CompletedActions[i]
		if c.Stage == stage {
			out = append(out, c.Action)
		}
	}
	return out
}

// loadLifetimeLedger returns the StageLedger for ss, creating an empty one when
// the spec has Lifetime actions but no ledger exists yet. It returns nil when
// there is neither a ledger nor any Lifetime action — the common case, which
// touches no extra API objects. The created ledger carries NO ownerReference:
// it must survive the StageSet's deletion (retain-always).
func (r *StageSetReconciler) loadLifetimeLedger(ctx context.Context, ss *stagesv1.StageSet) (*stagesv1.StageLedger, error) {
	var ledger stagesv1.StageLedger
	err := r.Get(ctx, types.NamespacedName{Namespace: ss.Namespace, Name: ss.Name}, &ledger)
	if err == nil {
		return &ledger, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, err
	}
	if !specHasLifetimeActions(ss) {
		return nil, nil
	}
	ledger = stagesv1.StageLedger{
		ObjectMeta: metav1.ObjectMeta{Namespace: ss.Namespace, Name: ss.Name},
	}
	if err := r.Create(ctx, &ledger); err != nil {
		return nil, err
	}
	return &ledger, nil
}

// promoteBaseline reconciles spec.baseline into status.completedActions: each
// entry that resolves to a real Lifetime action and is not already recorded is
// added with origin Baselined. It is additive-only (a removed spec entry never
// revokes a completion) and never downgrades an Executed completion. Entries
// that resolve to nothing are skipped, not rejected — a StageSet may be applied
// after its ledger, so resolution is a reconcile-time concern, not an admission
// one. Persists once if anything changed.
func (r *StageSetReconciler) promoteBaseline(ctx context.Context, ss *stagesv1.StageSet, ledger *stagesv1.StageLedger) error {
	if ledger == nil {
		return nil
	}
	changed := false
	for _, ref := range ledger.Spec.Baseline {
		if !isLifetimeAction(ss, ref.Stage, ref.Action) {
			continue // unresolvable (yet): held, re-evaluated next reconcile
		}
		if ledger.IsCompleted(ref.Stage, ref.Action) {
			continue // Executed (or already Baselined) outranks a fresh baseline
		}
		ledger.Status.CompletedActions = append(ledger.Status.CompletedActions, stagesv1.LedgerCompletion{
			Stage:       ref.Stage,
			Action:      ref.Action,
			Origin:      stagesv1.OriginBaselined,
			CompletedAt: metav1.Time{Time: r.now()},
		})
		changed = true
	}
	if changed {
		return r.Status().Update(ctx, ledger)
	}
	return nil
}

// recordLifetimeCompletion appends an Executed completion for (stage, action)
// and persists it immediately, before the next action runs — narrowing the
// crash window between a destructive bootstrap's side effect and its record as
// far as an anchor-less ledger allows. A persist failure is returned so the
// action fails loudly rather than silently losing the record.
func (r *StageSetReconciler) recordLifetimeCompletion(ctx context.Context, ledger *stagesv1.StageLedger, stage, action string) error {
	if ledger == nil {
		// Unreachable: a Lifetime action implies specHasLifetimeActions, which
		// materializes the ledger. Guard rather than nil-panic.
		return apierrors.NewInternalError(errNoLifetimeLedger)
	}
	if ledger.IsCompleted(stage, action) {
		return nil
	}
	ledger.Status.CompletedActions = append(ledger.Status.CompletedActions, stagesv1.LedgerCompletion{
		Stage:       stage,
		Action:      action,
		Origin:      stagesv1.OriginExecuted,
		CompletedAt: metav1.Time{Time: r.now()},
	})
	return r.Status().Update(ctx, ledger)
}
