/*
 * SPDX-FileCopyrightText: The stageset-controller Authors
 * SPDX-License-Identifier: 0BSD
 */

package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

// eventVerdict names the first event check whose pods exceeded their maxEvents.
// Produced only on a breach; a nil verdict means every check is within tolerance.
type eventVerdict struct {
	check    string
	observed int32
	rollback bool
}

// evaluateEventChecks runs a stage's promotion.eventGate against the apply-target
// cluster and returns a verdict for the first group whose pods accumulate more
// than maxEvents matching Warning events, or nil if all are within tolerance. A
// list error is returned so the caller can treat it as transient and retry rather
// than promote or block blind.
func (r *StageSetReconciler) evaluateEventChecks(ctx context.Context, target client.Reader, ss *stagesv1.StageSet, stage *stagesv1.Stage) (*eventVerdict, error) {
	if stage.Promotion == nil || stage.Promotion.EventGate == nil {
		return nil, nil
	}
	gate := stage.Promotion.EventGate
	for i := range gate.Checks {
		check := &gate.Checks[i]
		total, err := podWarningEventTotal(ctx, target, ss.Namespace, &check.Selector, check.Reasons)
		if err != nil {
			return nil, fmt.Errorf("event check %q: %w", check.Name, err)
		}
		if total > check.MaxEvents {
			onFailure := check.OnFailure
			if onFailure == "" {
				onFailure = gate.OnFailure
			}
			return &eventVerdict{check: check.Name, observed: total, rollback: onFailure == "Rollback"}, nil
		}
	}
	return nil, nil
}

// podWarningEventTotal sums the occurrence counts of Warning events whose reason
// is in the allow-list, over the pods matching the selector, in the namespace on
// the target cluster. Events are scoped to the exact current pod incarnations by
// involvedObject UID, so a previous revision's pods (different UID) don't count —
// which keeps the tally to this revision's behaviour without tracking soak start.
func podWarningEventTotal(ctx context.Context, target client.Reader, namespace string, ls *metav1.LabelSelector, reasons []string) (int32, error) {
	sel, err := metav1.LabelSelectorAsSelector(ls)
	if err != nil {
		return 0, fmt.Errorf("invalid selector: %w", err)
	}
	// An empty selector matches every pod in the namespace; never attribute
	// unrelated pods to the stage. Admission rejects this, so it is a guard.
	if sel.Empty() {
		return 0, nil
	}

	var pods corev1.PodList
	if err := target.List(ctx, &pods, client.InNamespace(namespace), client.MatchingLabelsSelector{Selector: sel}); err != nil {
		return 0, fmt.Errorf("list pods: %w", err)
	}
	if len(pods.Items) == 0 {
		return 0, nil
	}
	uids := make(map[types.UID]struct{}, len(pods.Items))
	for i := range pods.Items {
		uids[pods.Items[i].UID] = struct{}{}
	}
	allowed := make(map[string]struct{}, len(reasons))
	for _, reason := range reasons {
		allowed[reason] = struct{}{}
	}

	var events corev1.EventList
	if err := target.List(ctx, &events, client.InNamespace(namespace)); err != nil {
		return 0, fmt.Errorf("list events: %w", err)
	}
	var total int32
	for i := range events.Items {
		e := &events.Items[i]
		if e.Type != corev1.EventTypeWarning || e.InvolvedObject.Kind != "Pod" {
			continue
		}
		if _, ok := uids[e.InvolvedObject.UID]; !ok {
			continue
		}
		if _, ok := allowed[e.Reason]; !ok {
			continue
		}
		// Count aggregates repeated occurrences; treat an unset count as one.
		total += max(e.Count, 1)
	}
	return total, nil
}
