// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

// captureInventory snapshots every StageInventory a StageSet owns, the way a
// backup tool that restores spec would: it keeps the labels (the prune diff keys
// on them) and the spec (where the entries deliberately live, so a restore brings
// the prune history back).
func captureInventory(t *testing.T, c client.Client, ns, ssName string) []stagesv1.StageInventory {
	t.Helper()
	var list stagesv1.StageInventoryList
	if err := c.List(context.Background(), &list, client.InNamespace(ns), client.MatchingLabels{
		stagesv1.StageSetLabel: ssName,
	}); err != nil {
		t.Fatalf("list StageInventory: %v", err)
	}
	out := make([]stagesv1.StageInventory, 0, len(list.Items))
	for i := range list.Items {
		src := &list.Items[i]
		out = append(out, stagesv1.StageInventory{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:   src.Namespace,
				Name:        src.Name,
				Labels:      src.Labels,
				Annotations: src.Annotations,
			},
			Spec: *src.Spec.DeepCopy(),
		})
	}
	return out
}

// deleteInventory removes every StageInventory a StageSet owns, simulating the
// loss of cluster-only state that no Git source or live object can rebuild.
func deleteInventory(t *testing.T, c client.Client, ns, ssName string) {
	t.Helper()
	var list stagesv1.StageInventoryList
	if err := c.List(context.Background(), &list, client.InNamespace(ns), client.MatchingLabels{
		stagesv1.StageSetLabel: ssName,
	}); err != nil {
		t.Fatalf("list StageInventory: %v", err)
	}
	for i := range list.Items {
		// Drop the owner reference so the envtest garbage collector (absent
		// here) is irrelevant and the delete is unconditional.
		obj := &list.Items[i]
		obj.OwnerReferences = nil
		if err := c.Update(context.Background(), obj); err != nil {
			t.Fatalf("clear inventory owner refs: %v", err)
		}
		if err := c.Delete(context.Background(), obj); err != nil {
			t.Fatalf("delete StageInventory %s: %v", obj.Name, err)
		}
	}
}

// restoreInventory re-creates StageInventory objects from a captured backup,
// recovering the prune history a backup tool would have preserved.
func restoreInventory(t *testing.T, c client.Client, captured []stagesv1.StageInventory) {
	t.Helper()
	for i := range captured {
		obj := captured[i].DeepCopy()
		obj.ResourceVersion = ""
		obj.UID = ""
		if err := c.Create(context.Background(), obj); err != nil {
			t.Fatalf("restore StageInventory %s: %v", obj.Name, err)
		}
	}
}

// TestReconcile_StageInventoryLoss_SelfHealsViaReconstruction proves the
// conservative self-heal: when a stage's StageInventory is lost while its objects
// are still live, the next reconcile rebuilds the inventory from the cluster
// (folding the still-live objects back in by their owner + stage labels) and
// DEFERS pruning that pass; the following reconcile, now that the inventory is
// authoritative again, prunes what the render dropped. So a lost inventory
// recovers without a backup and without deleting against a best-effort rebuild.
func TestReconcile_StageInventoryLoss_SelfHealsViaReconstruction(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)

	servedArtifact(t, c, ns, "bundle", "", map[string]string{
		"a.yaml": configMapManifest(ns, "cm-a"),
		"b.yaml": configMapManifest(ns, "cm-b"),
	})
	ss := newStageSet(t, c, ns, "selfheal", stagesv1.SourceReference{Name: "bundle"})
	reconcileOnce(t, c, ss)

	if !cmExists(t, c, ns, "cm-a") || !cmExists(t, c, ns, "cm-b") {
		t.Fatal("first run should apply both ConfigMaps")
	}
	if n := inventoryEntryCount(t, c, ns, "selfheal", "stage-a"); n != 2 {
		t.Fatalf("inventory after run 1 = %d entries, want 2", n)
	}

	// Lose the inventory while cm-a and cm-b are still live and labeled.
	deleteInventory(t, c, ns, "selfheal")
	if n := inventoryEntryCount(t, c, ns, "selfheal", "stage-a"); n != 0 {
		t.Fatalf("inventory should be gone, got %d entries", n)
	}

	// The render now drops cm-b. The first reconcile after loss reconstructs the
	// inventory from the live objects and defers pruning.
	repointArtifact(t, c, ns, "bundle", map[string]string{"a.yaml": configMapManifest(ns, "cm-a")})
	reconcileOnce(t, c, ss)

	if !cmExists(t, c, ns, "cm-b") {
		t.Fatal("cm-b must survive the reconstruction pass — pruning is deferred, not run against a best-effort rebuild")
	}
	if n := inventoryEntryCount(t, c, ns, "selfheal", "stage-a"); n != 2 {
		t.Fatalf("reconstructed inventory = %d entries, want 2 (cm-a plus the still-live cm-b folded back from the cluster)", n)
	}

	// The next reconcile, with the inventory authoritative again, prunes the
	// object the render dropped.
	reconcileOnce(t, c, ss)
	if !cmExists(t, c, ns, "cm-a") {
		t.Fatal("cm-a should survive")
	}
	if cmExists(t, c, ns, "cm-b") {
		t.Fatal("cm-b should be pruned on the reconcile after reconstruction")
	}
	if n := inventoryEntryCount(t, c, ns, "selfheal", "stage-a"); n != 1 {
		t.Fatalf("inventory after prune = %d entries, want 1", n)
	}
}

// TestReconcile_StageInventory_RestoredBackupPrunesImmediately proves the backup
// path: restoring a captured StageInventory before a reconcile makes the stored
// record authoritative, so the very next reconcile prunes normally (no
// reconstruction, no deferral). This is the cluster-rebuild checklist's path.
func TestReconcile_StageInventory_RestoredBackupPrunesImmediately(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)

	servedArtifact(t, c, ns, "bundle", "", map[string]string{
		"a.yaml": configMapManifest(ns, "cm-a"),
		"b.yaml": configMapManifest(ns, "cm-b"),
	})
	ss := newStageSet(t, c, ns, "restored", stagesv1.SourceReference{Name: "bundle"})
	reconcileOnce(t, c, ss)

	if !cmExists(t, c, ns, "cm-a") || !cmExists(t, c, ns, "cm-b") {
		t.Fatal("first run should apply both ConfigMaps")
	}

	backup := captureInventory(t, c, ns, "restored")
	if len(backup) == 0 {
		t.Fatal("expected to capture at least one StageInventory")
	}
	deleteInventory(t, c, ns, "restored")
	restoreInventory(t, c, backup)
	if n := inventoryEntryCount(t, c, ns, "restored", "stage-a"); n != 2 {
		t.Fatalf("restored inventory = %d entries, want 2", n)
	}

	// With the inventory restored, the B-removing render prunes on this reconcile.
	repointArtifact(t, c, ns, "bundle", map[string]string{"a.yaml": configMapManifest(ns, "cm-a")})
	reconcileOnce(t, c, ss)

	if !cmExists(t, c, ns, "cm-a") {
		t.Fatal("cm-a should survive")
	}
	if cmExists(t, c, ns, "cm-b") {
		t.Fatal("cm-b should be PRUNED — a restored inventory backup makes pruning correct immediately")
	}
	if n := inventoryEntryCount(t, c, ns, "restored", "stage-a"); n != 1 {
		t.Fatalf("inventory after restore+prune = %d entries, want 1", n)
	}
}

// TestStageSet_RejectsUpperCaseStageName pins the invariant the label-based
// reconstruction relies on: stage names are lowercase. The CRD pattern
// `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$` rejects an upper-case name at admission, so a
// stage label and the reconstruction selector that matches it are always the same
// (already-lowercase) value — no case normalisation is needed or correct.
func TestStageSet_RejectsUpperCaseStageName(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "uppercase-stage"},
		Spec: stagesv1.StageSetSpec{
			Interval: metav1.Duration{Duration: 5 * time.Minute},
			Stages: []stagesv1.Stage{{
				Name:      "Stage-A", // upper-case: must be rejected at admission
				SourceRef: stagesv1.SourceReference{Name: "bundle"},
			}},
		},
	}
	err := c.Create(context.Background(), ss)
	if err == nil {
		t.Fatal("apiserver must reject an upper-case stage name; the reconstruction label match depends on stage names being lowercase")
	}
	if !apierrors.IsInvalid(err) {
		t.Fatalf("want an Invalid validation error for the stage-name pattern, got %T: %v", err, err)
	}
}
