// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package apply

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// StampStageLabel stamps the per-stage discovery label (stageLabelKey=stage) on
// every object. Members already carry the StageSet-level owner labels server-side
// apply stamps (stages.metio.wtf/name, /namespace); this adds the per-stage
// dimension, so `kubectl get <type> -l <stageLabelKey>=<stage>` enumerates exactly
// one stage's objects with no project-specific tooling.
//
// Both the reconcile apply and the CLI dry-run diff stamp through here so the
// preview matches what an apply writes; a divergence would render the label as
// spurious churn on every diff.
func StampStageLabel(objects []*unstructured.Unstructured, stageLabelKey, stage string) {
	for _, o := range objects {
		labels := o.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		labels[stageLabelKey] = stage
		o.SetLabels(labels)
	}
}
