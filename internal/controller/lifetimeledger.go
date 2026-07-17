// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

// errNoLifetimeLedger guards the unreachable nil-ledger path in
// recordLifetimeCompletion; a Lifetime action always materializes its ledger.
var errNoLifetimeLedger = errors.New("scope: Lifetime action has no StageLedger to record into")

// eventBaselineInvalid is the Warning event reason emitted when a spec.baseline
// entry does not resolve to a scope: Lifetime action. It fires on the
// transition into the invalid state (and on a change of which entries are
// unresolved), not every reconcile, so a persistent typo is loud once, not a
// storm.
const eventBaselineInvalid = "BaselineInvalid"

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
	var unresolved []string
	for _, ref := range ledger.Spec.Baseline {
		if !isLifetimeAction(ss, ref.Stage, ref.Action) {
			// Held, not dropped: an entry that resolves to no scope: Lifetime action
			// is surfaced (below) and promoted automatically once a later spec change
			// makes it resolvable. A StageSet may be applied after its ledger, so
			// resolution is a reconcile-time concern, not an admission one.
			unresolved = append(unresolved, ref.Stage+"/"+ref.Action)
			continue
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
	if r.reconcileBaselineCondition(ss, ledger, unresolved) {
		changed = true
	}
	if changed {
		return r.Status().Update(ctx, ledger)
	}
	return nil
}

// reconcileBaselineCondition reflects the set of unresolvable spec.baseline
// entries as the ledger's BaselineValid condition, emitting a Warning event on
// the transition into (or a change within) the invalid state. It returns
// whether the condition changed. It manages the condition only when there is
// something to say — baseline entries present, or a stale condition to clear —
// so a ledger with no baseline carries no condition noise.
func (r *StageSetReconciler) reconcileBaselineCondition(ss *stagesv1.StageSet, ledger *stagesv1.StageLedger, unresolved []string) bool {
	if len(ledger.Spec.Baseline) == 0 && apimeta.FindStatusCondition(ledger.Status.Conditions, stagesv1.LedgerConditionBaselineValid) == nil {
		return false
	}
	cond := metav1.Condition{
		Type:               stagesv1.LedgerConditionBaselineValid,
		ObservedGeneration: ledger.Generation,
		LastTransitionTime: metav1.Time{Time: r.now()},
	}
	if len(unresolved) == 0 {
		cond.Status = metav1.ConditionTrue
		cond.Reason = "AllResolved"
		cond.Message = "every spec.baseline entry resolves to a scope: Lifetime action"
	} else {
		cond.Status = metav1.ConditionFalse
		cond.Reason = "Unresolved"
		cond.Message = fmt.Sprintf("spec.baseline entries that resolve to no scope: Lifetime action in StageSet %q: %s", ss.Name, strings.Join(unresolved, ", "))
	}

	prior := apimeta.FindStatusCondition(ledger.Status.Conditions, stagesv1.LedgerConditionBaselineValid)
	transitioned := prior == nil || prior.Status != cond.Status || prior.Message != cond.Message
	changed := apimeta.SetStatusCondition(&ledger.Status.Conditions, cond)
	if transitioned && cond.Status == metav1.ConditionFalse && r.Recorder != nil {
		r.Recorder.Eventf(ledger, nil, corev1.EventTypeWarning, eventBaselineInvalid, eventBaselineInvalid, "%s", cond.Message)
	}
	return changed
}

// recordLifetimeCompletion appends an Executed completion for (stage, action)
// and persists it immediately, before the next action runs — narrowing the
// crash window between a destructive bootstrap's side effect and its record as
// far as an anchor-less ledger allows. A persist failure is returned so the
// action fails loudly rather than silently losing the record.
//
// When the action declared a completionAnchor, the witness object is read
// (through the stage's effective-SA client) and its UID recorded on the
// completion. The anchor must exist at completion so its UID can be captured; an
// absent or unreadable anchor fails the action — a UID-less anchored completion
// could never be invalidated, so recording one would be worse than not recording
// at all.
func (r *StageSetReconciler) recordLifetimeCompletion(ctx context.Context, stageClient client.Client, ledger *stagesv1.StageLedger, ns, stage, action string, anchor *stagesv1.CompletionAnchor) error {
	if ledger == nil {
		// Unreachable: a Lifetime action implies specHasLifetimeActions, which
		// materializes the ledger. Guard rather than nil-panic.
		return apierrors.NewInternalError(errNoLifetimeLedger)
	}
	if ledger.IsCompleted(stage, action) {
		return nil
	}
	completion := stagesv1.LedgerCompletion{
		Stage:       stage,
		Action:      action,
		Origin:      stagesv1.OriginExecuted,
		CompletedAt: metav1.Time{Time: r.now()},
	}
	if anchor != nil {
		obj, err := readAnchorObject(ctx, stageClient, ns, anchor.APIVersion, anchor.Kind, anchor.Name)
		if err != nil {
			return fmt.Errorf("completionAnchor %s %s/%s for action %q/%q could not be read at completion (it must exist so its UID can be recorded): %w", anchor.Kind, ns, anchor.Name, stage, action, err)
		}
		completion.Anchor = &stagesv1.AnchorWitness{
			APIVersion: anchor.APIVersion,
			Kind:       anchor.Kind,
			Name:       anchor.Name,
			UID:        string(obj.GetUID()),
		}
	}
	ledger.Status.CompletedActions = append(ledger.Status.CompletedActions, completion)
	return r.Status().Update(ctx, ledger)
}
