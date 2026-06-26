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

// evaluateRestartChecks runs a stage's promotion.restartChecks against the
// apply-target cluster and returns a verdict for the first group whose pods
// exceed their maxRestarts, or nil if all are within tolerance. A list error is
// returned so the caller can treat it as a transient and retry rather than
// promote or block blind.
func (r *StageSetReconciler) evaluateRestartChecks(ctx context.Context, target client.Reader, ss *stagesv1.StageSet, stage *stagesv1.Stage) (*restartVerdict, error) {
	if stage.Promotion == nil || stage.Promotion.RestartGate == nil {
		return nil, nil
	}
	gate := stage.Promotion.RestartGate
	for i := range gate.Checks {
		check := &gate.Checks[i]
		total, err := podRestartTotal(ctx, target, ss.Namespace, &check.Selector)
		if err != nil {
			return nil, fmt.Errorf("restart check %q: %w", check.Name, err)
		}
		if total > check.MaxRestarts {
			onFailure := check.OnFailure
			if onFailure == "" {
				onFailure = gate.OnFailure
			}
			return &restartVerdict{check: check.Name, observed: total, rollback: onFailure == "Rollback"}, nil
		}
	}
	return nil, nil
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
		st := pods.Items[i].Status
		for _, cs := range st.InitContainerStatuses {
			total += cs.RestartCount
		}
		for _, cs := range st.ContainerStatuses {
			total += cs.RestartCount
		}
	}
	return total, nil
}
