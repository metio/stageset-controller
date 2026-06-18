// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

// TestStageLabel_StampedOnEveryMember pins that every applied object carries the
// per-stage discovery label (stages.metio.wtf/stage=<stage>) alongside the
// StageSet-level owner labels, so `kubectl get <type> -l stages.metio.wtf/stage=<stage>`
// enumerates exactly one stage's objects. (Pruning and teardown are covered by
// the inventory tests; they are independent of this label.)
func TestStageLabel_StampedOnEveryMember(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	servedArtifact(t, c, ns, "bundle", "", map[string]string{
		"a.yaml": configMapManifest(ns, "cm-a"),
		"b.yaml": configMapManifest(ns, "cm-b"),
	})
	ss := newStageSet(t, c, ns, "labeler", stagesv1.SourceReference{Name: "bundle"})
	reconcileOnce(t, c, ss)

	for _, name := range []string{"cm-a", "cm-b"} {
		var cm corev1.ConfigMap
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &cm); err != nil {
			t.Fatalf("get %s: %v", name, err)
		}
		if got := cm.Labels[stagesv1.StageLabel]; got != "stage-a" {
			t.Errorf("%s %s = %q, want stage-a", name, stagesv1.StageLabel, got)
		}
		// The StageSet-level owner label is present too.
		if got := cm.Labels["stages.metio.wtf/name"]; got != "labeler" {
			t.Errorf("%s stages.metio.wtf/name = %q, want labeler", name, got)
		}
	}
}
