// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

// cmValManifest is a mutable ConfigMap with a controllable data value.
func cmValManifest(ns, name, val string) string {
	return "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: " + name + "\n  namespace: " + ns + "\ndata:\n  key: " + val + "\n"
}

// A failure after an earlier stage already applied new content rolls the whole
// run back to the last-good snapshot, so the earlier stage's object returns to
// its previous value.
func TestReconcile_RollbackOnFailure_RestoresPreviousRevision(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	servedArtifact(t, c, ns, "ea-a", "", map[string]string{"cm.yaml": cmValManifest(ns, "shared", "v1")})
	servedArtifact(t, c, ns, "ea-b", "", map[string]string{"cm.yaml": configMapManifest(ns, "obj-b")})

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "rb"},
		Spec: stagesv1.StageSetSpec{
			Interval:          metav1.Duration{Duration: time.Minute},
			RollbackOnFailure: true,
			Stages: []stagesv1.Stage{
				{Name: "stage-a", SourceRef: stagesv1.SourceReference{Name: "ea-a"}},
				{Name: "stage-b", SourceRef: stagesv1.SourceReference{Name: "ea-b"}},
			},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	reconcileOnce(t, c, ss) // success: snapshot {shared=v1, obj-b}
	if cmDataKey(t, c, ns, "shared") != "v1" {
		t.Fatal("first run should apply shared=v1")
	}

	// Stage A rolls forward to v2 (good); stage B becomes a manifest the
	// apiserver rejects (invalid name), failing the run after A applied v2.
	repointArtifact(t, c, ns, "ea-a", map[string]string{"cm.yaml": cmValManifest(ns, "shared", "v2")})
	repointArtifact(t, c, ns, "ea-b", map[string]string{"cm.yaml": cmValManifest(ns, "Bad_Name", "x")})
	_ = reconcileWith(t, c, ss, nil)

	got := getStageSet(t, c, ns, "rb")
	if readyReason(got) != ReasonStageFailed {
		t.Fatalf("Ready reason = %q, want %q", readyReason(got), ReasonStageFailed)
	}
	if v := cmDataKey(t, c, ns, "shared"); v != "v1" {
		t.Fatalf("rollback should have restored shared to v1, got %q", v)
	}
}

// A first run that fails has no snapshot to roll back to: it just fails, and no
// rollback Secret is left behind.
func TestReconcile_RollbackOnFailure_FirstRunHasNothingToRestore(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	servedArtifact(t, c, ns, "ea", "", map[string]string{"cm.yaml": cmValManifest(ns, "Bad_Name", "x")})

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "rb-first"},
		Spec: stagesv1.StageSetSpec{
			Interval:          metav1.Duration{Duration: time.Minute},
			RollbackOnFailure: true,
			Stages:            []stagesv1.Stage{{Name: "stage-a", SourceRef: stagesv1.SourceReference{Name: "ea"}}},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	_ = reconcileWith(t, c, ss, nil)

	got := getStageSet(t, c, ns, "rb-first")
	if readyReason(got) != ReasonStageFailed {
		t.Fatalf("Ready reason = %q, want %q", readyReason(got), ReasonStageFailed)
	}
	if len(got.Status.LastAppliedSnapshot) != 0 {
		t.Fatalf("no rollback snapshot should be recorded after a failed first run, got %d", len(got.Status.LastAppliedSnapshot))
	}
}
