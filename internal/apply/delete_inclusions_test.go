// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package apply_test

import (
	"context"
	"testing"

	"github.com/fluxcd/pkg/ssa"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/metio/stageset-controller/internal/apply"
)

func actionFor(t *testing.T, cs *ssa.ChangeSet, subjectSuffix string) ssa.Action {
	t.Helper()
	for _, e := range cs.Entries {
		if len(e.Subject) >= len(subjectSuffix) && e.Subject[len(e.Subject)-len(subjectSuffix):] == subjectSuffix {
			return e.Action
		}
	}
	t.Fatalf("no changeset entry for %q in %v", subjectSuffix, cs.Entries)
	return ""
}

// Delete passes the StageSet's owner labels as ssa Inclusions, so an object that
// still carries them is pruned, while an object whose owner labels have been
// stripped (ownership transferred to another manager) is left untouched.
func TestDelete_PrunesOnlyStillOwnedObjects(t *testing.T) {
	if testCfg == nil {
		t.Skip("envtest assets unavailable")
	}
	a, c := applierFor(t)
	ctx := context.Background()

	owned := configMap("owned", map[string]any{"k": "v"})
	transferred := configMap("transferred", map[string]any{"k": "v"})

	// Apply both: ssa stamps owner labels (stages.metio.wtf/name=ss,
	// stages.metio.wtf/namespace=default).
	if _, err := a.Apply(ctx, "ss", "default", []*unstructured.Unstructured{owned, transferred}, apply.ConflictHandling{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Simulate another manager adopting "transferred": strip the owner labels
	// from the LIVE object.
	var live unstructured.Unstructured
	live.SetGroupVersionKind(transferred.GroupVersionKind())
	if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "transferred"}, &live); err != nil {
		t.Fatalf("get transferred: %v", err)
	}
	labels := live.GetLabels()
	delete(labels, "stages.metio.wtf/name")
	delete(labels, "stages.metio.wtf/namespace")
	live.SetLabels(labels)
	if err := c.Update(ctx, &live); err != nil {
		t.Fatalf("strip owner labels: %v", err)
	}

	// Prune both as if they fell out of the inventory.
	cs, err := a.Delete(ctx, "ss", "default", []*unstructured.Unstructured{
		configMap("owned", nil), configMap("transferred", nil),
	})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// The still-owned object is deleted; the relabeled one is skipped.
	if got := actionFor(t, cs, "/owned"); got != ssa.DeletedAction {
		t.Fatalf("owned object: action = %q, want %q", got, ssa.DeletedAction)
	}
	if got := actionFor(t, cs, "/transferred"); got != ssa.SkippedAction {
		t.Fatalf("transferred object: action = %q, want %q", got, ssa.SkippedAction)
	}

	// The skipped object must still exist; the deleted one must be gone.
	var probe unstructured.Unstructured
	probe.SetGroupVersionKind(transferred.GroupVersionKind())
	if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "transferred"}, &probe); err != nil {
		t.Fatalf("transferred object must survive prune: %v", err)
	}
	probe = unstructured.Unstructured{}
	probe.SetGroupVersionKind(owned.GroupVersionKind())
	if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "owned"}, &probe); err == nil {
		t.Fatal("owned object should have been pruned")
	}
}
