// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package apply

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/metio/stageset-controller/internal/inventory"
)

// StampMemberLabels stamps the ApplySet member label (part-of) on every object
// when mode is "hybrid" or "applyset"; in "entries" mode it is a no-op. The
// value is the ApplySet ID of the stage's shard-zero StageInventory parent, so
// `kubectl get -l applyset.kubernetes.io/part-of=<id>` enumerates a stage's
// members with no project-specific tooling.
//
// Both the reconcile apply and the CLI dry-run diff stamp through here so the
// preview is faithful to what an apply writes; a divergence renders the part-of
// label as spurious churn on every diff of a hybrid/applyset StageSet.
func StampMemberLabels(objects []*unstructured.Unstructured, mode, stageSet, stage, namespace, group string) {
	if mode != "hybrid" && mode != "applyset" {
		return
	}
	id := inventory.ApplySetID(inventory.ShardName(stageSet, stage, 0), namespace, "StageInventory", group)
	for _, o := range objects {
		labels := o.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		labels[inventory.PartOfLabel] = id
		o.SetLabels(labels)
	}
}
