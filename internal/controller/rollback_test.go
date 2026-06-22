// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

// cmValManifest is a mutable ConfigMap with a controllable data value.
func cmValManifest(ns, name, val string) string {
	return "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: " + name + "\n  namespace: " + ns + "\ndata:\n  key: " + val + "\n"
}

// substitutionDigest fingerprints the resolved substitution inputs so rollback
// can refuse to restore when they changed. The fingerprint must be empty for an
// empty map (disables the check), deterministic and order-independent, and free
// of key/value-boundary collisions.
func TestSubstitutionDigest(t *testing.T) {
	if d := substitutionDigest(nil); d != "" {
		t.Errorf("nil map digest = %q, want empty (check disabled)", d)
	}
	if d := substitutionDigest(map[string]string{}); d != "" {
		t.Errorf("empty map digest = %q, want empty", d)
	}
	if a, b := substitutionDigest(map[string]string{"x": "1", "y": "2"}),
		substitutionDigest(map[string]string{"y": "2", "x": "1"}); a == "" || a != b {
		t.Errorf("digest must be deterministic and order-independent: %q vs %q", a, b)
	}
	if substitutionDigest(map[string]string{"ab": "c"}) == substitutionDigest(map[string]string{"a": "bc"}) {
		t.Error("length-prefixing must prevent a key/value-boundary collision")
	}
	if substitutionDigest(map[string]string{"x": "1"}) == substitutionDigest(map[string]string{"x": "2"}) {
		t.Error("a changed value must change the digest")
	}
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
	// The re-fetch rollback path must stamp the per-stage discovery label, so the
	// restored object stays selectable by stages.metio.wtf/stage (it isn't store-
	// backed here — no rollback store is configured).
	restored := &corev1.ConfigMap{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "shared"}, restored); err != nil {
		t.Fatalf("get restored ConfigMap: %v", err)
	}
	if got := restored.Labels[stagesv1.StageLabel]; got != "stage-a" {
		t.Errorf("restored ConfigMap %s=%q, want stage-a", stagesv1.StageLabel, got)
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
