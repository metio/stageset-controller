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
	"sigs.k8s.io/controller-runtime/pkg/client"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

// restartVerdict names the first restart check whose pods exceeded their
// maxRestarts. It is produced only on a breach; a nil verdict means every check
// is within tolerance (or none are configured).
type restartVerdict struct {
	check    string
	observed int32
	rollback bool
}

// restartObservation is one pass of the restart gate: the breach it found (nil
// when every check is within tolerance), the baseline it measured against, and
// the live totals — which the caller adopts as the next baseline once the stage
// promotes.
type restartObservation struct {
	verdict  *restartVerdict
	baseline []stagesv1.RestartCheckBaseline
	totals   []stagesv1.RestartCheckBaseline
}

// evaluateRestartChecks runs a stage's promotion.restartGate against the
// apply-target cluster and reports the first group whose pods exceed their
// maxRestarts. A list error is returned so the caller can treat it as a transient
// and retry rather than promote or block blind.
//
// RestartCount is cumulative over a pod's life, and a rollout does not always
// replace the pods a check watches — a stage that only patches a ConfigMap leaves
// them running, and a StatefulSet's may outlive many revisions. Measuring the raw
// total would charge a deploy for every restart those pods have ever had, so a
// workload with a few weeks of restarts behind it fails the gate the next time it
// ships and, under onFailure: Rollback, reverts a revision that is perfectly
// healthy. Each check is therefore judged on the EXCESS over a baseline.
//
// The baseline is the total observed at the stage's LAST PROMOTION, not at the
// current revision's arrival. Keying it to the revision would look right and be
// useless: the gate is only consulted while a stage is un-promoted, so every
// evaluation would follow a fresh capture and no breach could ever land on the
// pass a revision arrives. Anchoring at the last promotion instead means the
// window is "since this stage was last known good" — restarts a new revision
// walks into are counted immediately, while everything before the last clean
// promotion is forgiven.
func (r *StageSetReconciler) evaluateRestartChecks(ctx context.Context, target client.Reader, ss *stagesv1.StageSet, stage *stagesv1.Stage, prior stagesv1.StageStatus) (restartObservation, error) {
	if stage.Promotion == nil || stage.Promotion.RestartGate == nil {
		return restartObservation{}, nil
	}
	gate := stage.Promotion.RestartGate
	carried := priorRestartBaseline(prior)
	obs := restartObservation{
		baseline: make([]stagesv1.RestartCheckBaseline, 0, len(gate.Checks)),
		totals:   make([]stagesv1.RestartCheckBaseline, 0, len(gate.Checks)),
	}
	for i := range gate.Checks {
		check := &gate.Checks[i]
		total, err := podRestartTotal(ctx, target, ss.Namespace, &check.Selector)
		if err != nil {
			return restartObservation{}, fmt.Errorf("restart check %q: %w", check.Name, err)
		}
		base, seen := carried[check.Name]
		if !seen {
			// Never observed: nothing that predates this StageSet is charged to it.
			base = total
		}
		// Pods replaced since the baseline reset their counters, so the live total
		// can fall below it. Clamp rather than report negative progress.
		excess := max(total-base, 0)
		obs.baseline = append(obs.baseline, stagesv1.RestartCheckBaseline{Name: check.Name, Restarts: base})
		obs.totals = append(obs.totals, stagesv1.RestartCheckBaseline{Name: check.Name, Restarts: total})
		// Keep going so every check's baseline is recorded, but report the FIRST
		// breach — the checks are ordered and the message names one cause.
		if excess > check.MaxRestarts && obs.verdict == nil {
			onFailure := check.OnFailure
			if onFailure == "" {
				onFailure = gate.OnFailure
			}
			obs.verdict = &restartVerdict{check: check.Name, observed: excess, rollback: onFailure == "Rollback"}
		}
	}
	return obs, nil
}

// priorRestartBaseline returns the per-check baselines the stage carries, or nil
// when it has none yet. It deliberately ignores the applied revision: the window
// the gate measures is "since the last promotion", which spans revisions.
func priorRestartBaseline(prior stagesv1.StageStatus) map[string]int32 {
	if prior.PromotionState == nil {
		return nil
	}
	out := make(map[string]int32, len(prior.PromotionState.RestartBaseline))
	for _, b := range prior.PromotionState.RestartBaseline {
		out[b.Name] = b.Restarts
	}
	return out
}

// podRestartTotal sums the container restart counts of every pod matching the
// selector in the namespace on the target cluster. Matching pods directly by
// label — rather than walking owner kinds — keeps the check source-agnostic:
// pods from Deployments, StatefulSets, Jobs, or a custom controller all count.
// Init- and regular-container restarts are both included.
func podRestartTotal(ctx context.Context, target client.Reader, namespace string, ls *metav1.LabelSelector) (int32, error) {
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

	var total int32
	for i := range pods.Items {
		pod := &pods.Items[i]
		// A terminating pod is draining from the prior revision (or a scale-down);
		// its accumulated RestartCount must not gate the revision replacing it.
		// RestartCount is cumulative over a pod's life, so without this skip an
		// old-revision pod's lifetime restarts would falsely trip the gate during
		// the rollout overlap.
		if pod.DeletionTimestamp != nil {
			continue
		}
		for _, cs := range pod.Status.InitContainerStatuses {
			total += cs.RestartCount
		}
		for _, cs := range pod.Status.ContainerStatuses {
			total += cs.RestartCount
		}
	}
	return total, nil
}
