// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package apply_test

import (
	"context"
	"testing"

	"github.com/fluxcd/cli-utils/pkg/object"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// A ConfigMap is Current the moment it exists, so a set of them is never
// stalled — the case a verify timeout has to be distinguishable from.
func TestStalled_HealthyObjectsAreNotStalled(t *testing.T) {
	applier, c := applierFor(t)
	cm := configMap("stalled-healthy", map[string]any{"k": "v"})
	if err := c.Create(context.Background(), cm); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = c.Delete(context.Background(), cm) })

	set := object.ObjMetadataSet{{
		GroupKind: schema.GroupKind{Kind: "ConfigMap"},
		Namespace: "default",
		Name:      "stalled-healthy",
	}}
	if applier.Stalled(context.Background(), set) {
		t.Error("a healthy ConfigMap reported as stalled; a verify timeout would wrongly roll back")
	}
}

// An object that cannot be read must not count as stalled. Callers decide
// whether to undo a release on this answer, so an unreadable object has to fall
// on the "keep waiting" side rather than trigger a rollback.
func TestStalled_UnreadableObjectsAreNotStalled(t *testing.T) {
	applier, _ := applierFor(t)
	set := object.ObjMetadataSet{
		{ // never created
			GroupKind: schema.GroupKind{Kind: "ConfigMap"},
			Namespace: "default",
			Name:      "stalled-absent",
		},
		{ // kind the mapper cannot resolve
			GroupKind: schema.GroupKind{Group: "nope.example.com", Kind: "Nothing"},
			Namespace: "default",
			Name:      "stalled-unmapped",
		},
	}
	if applier.Stalled(context.Background(), set) {
		t.Error("unreadable objects reported as stalled")
	}
}

// An empty set is the no-wait case and must answer false rather than panicking.
func TestStalled_EmptySet(t *testing.T) {
	applier, _ := applierFor(t)
	if applier.Stalled(context.Background(), object.ObjMetadataSet{}) {
		t.Error("empty set reported as stalled")
	}
}

// A Deployment whose Progressing condition reports ProgressDeadlineExceeded is
// kstatus's canonical terminal failure. envtest runs no controllers, so the
// status is written directly — which is the point: the classifier reads live
// status and owes nothing to how it got there.
func TestStalled_TerminallyFailedObjectIsStalled(t *testing.T) {
	applier, c := applierFor(t)
	ctx := context.Background()
	dep := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": "stalled-dep", "namespace": "default"},
		"spec": map[string]any{
			"replicas": int64(1),
			"selector": map[string]any{"matchLabels": map[string]any{"app": "stalled"}},
			"template": map[string]any{
				"metadata": map[string]any{"labels": map[string]any{"app": "stalled"}},
				"spec": map[string]any{"containers": []any{
					map[string]any{"name": "c", "image": "docker.io/library/busybox:latest"},
				}},
			},
		},
	}}
	if err := c.Create(ctx, dep); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = c.Delete(ctx, dep) })

	dep.Object["status"] = map[string]any{
		"observedGeneration": dep.GetGeneration(),
		"replicas":           int64(1),
		"updatedReplicas":    int64(1),
		"conditions": []any{map[string]any{
			"type": "Progressing", "status": "False",
			"reason":  "ProgressDeadlineExceeded",
			"message": "ReplicaSet has timed out progressing",
		}},
	}
	if err := c.Status().Update(ctx, dep); err != nil {
		t.Fatalf("status update: %v", err)
	}

	set := object.ObjMetadataSet{{
		GroupKind: schema.GroupKind{Group: "apps", Kind: "Deployment"},
		Namespace: "default",
		Name:      "stalled-dep",
	}}
	if !applier.Stalled(ctx, set) {
		t.Error("a Deployment past its progress deadline was not reported as stalled; a genuine failure would skip its rollback")
	}
}
