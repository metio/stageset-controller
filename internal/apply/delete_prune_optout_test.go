// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package apply_test

import (
	"context"
	"testing"

	"github.com/fluxcd/pkg/ssa"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/apply"
)

// Delete must honor the per-object prune opt-out: a live object annotated
// stages.metio.wtf/prune=disabled is skipped, matching the preview/prune path
// so `stageset diff` and the real teardown agree. An object without the
// annotation is pruned as usual.
func TestDelete_HonorsPruneDisabledAnnotation(t *testing.T) {
	if testCfg == nil {
		t.Skip("envtest assets unavailable")
	}
	a, c := applierFor(t)
	ctx := context.Background()

	prunable := configMap("prunable", map[string]any{"k": "v"})
	protected := configMap("protected", map[string]any{"k": "v"})

	if _, err := a.Apply(ctx, "ss", "default", []*unstructured.Unstructured{prunable, protected}, apply.ConflictHandling{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Annotate the live "protected" object to opt out of pruning, keeping its
	// owner labels intact so only the annotation can spare it.
	var live unstructured.Unstructured
	live.SetGroupVersionKind(protected.GroupVersionKind())
	if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "protected"}, &live); err != nil {
		t.Fatalf("get protected: %v", err)
	}
	ann := live.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	ann[stagesv1.PruneAnnotation] = "disabled"
	live.SetAnnotations(ann)
	if err := c.Update(ctx, &live); err != nil {
		t.Fatalf("annotate protected: %v", err)
	}

	cs, err := a.Delete(ctx, "ss", "default", []*unstructured.Unstructured{
		configMap("prunable", nil), configMap("protected", nil),
	})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if got := actionFor(t, cs, "/prunable"); got != ssa.DeletedAction {
		t.Fatalf("prunable object: action = %q, want %q", got, ssa.DeletedAction)
	}
	if got := actionFor(t, cs, "/protected"); got != ssa.SkippedAction {
		t.Fatalf("protected object: action = %q, want %q", got, ssa.SkippedAction)
	}

	var probe unstructured.Unstructured
	probe.SetGroupVersionKind(protected.GroupVersionKind())
	if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "protected"}, &probe); err != nil {
		t.Fatalf("prune-disabled object must survive teardown: %v", err)
	}
	probe = unstructured.Unstructured{}
	probe.SetGroupVersionKind(prunable.GroupVersionKind())
	if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "prunable"}, &probe); err == nil {
		t.Fatal("prunable object should have been pruned")
	}
}
