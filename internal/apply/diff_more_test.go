// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package apply_test

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/metio/stageset-controller/internal/apply"
)

const (
	ownerNameLabel = "stages.metio.wtf/name"
	ownerNsLabel   = "stages.metio.wtf/namespace"
)

func secret(name string, data map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata":   map[string]any{"name": name, "namespace": "default"},
		"stringData": data,
	}}
}

func namespaceObj(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Namespace",
		"metadata": map[string]any{"name": name},
	}}
}

// TestDiff_OwnerLabelsStampedOnMerged proves Diff stamps the owner labels on the
// merged preview for both create and configure, mirroring what a real apply writes.
func TestDiff_OwnerLabelsStampedOnMerged(t *testing.T) {
	if testCfg == nil {
		t.Skip("envtest assets unavailable")
	}
	a, c := applierFor(t)
	ctx := context.Background()

	// create: Merged carries the owner labels.
	entries, err := a.Diff(ctx, "owner-ss", "owner-ns",
		[]*unstructured.Unstructured{configMap("cfg-owner-create", map[string]any{"k": "v1"})}, apply.ConflictHandling{})
	if err != nil {
		t.Fatalf("Diff(create): %v", err)
	}
	if entries[0].Action != apply.DiffCreate || entries[0].Merged == nil {
		t.Fatalf("want create with merged, got %+v", entries[0])
	}
	labels := entries[0].Merged.GetLabels()
	if labels[ownerNameLabel] != "owner-ss" || labels[ownerNsLabel] != "owner-ns" {
		t.Fatalf("create merged missing owner labels: %v", labels)
	}

	// configure: apply first, then diff a changed value; Merged still carries labels.
	if _, err := a.Apply(ctx, "owner-ss", "owner-ns",
		[]*unstructured.Unstructured{configMap("cfg-owner-cfg", map[string]any{"k": "v1"})}, apply.ConflictHandling{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	entries, err = a.Diff(ctx, "owner-ss", "owner-ns",
		[]*unstructured.Unstructured{configMap("cfg-owner-cfg", map[string]any{"k": "v2"})}, apply.ConflictHandling{})
	if err != nil {
		t.Fatalf("Diff(configure): %v", err)
	}
	if entries[0].Action != apply.DiffConfigure || entries[0].Merged == nil {
		t.Fatalf("want configure with merged, got %+v", entries[0])
	}
	labels = entries[0].Merged.GetLabels()
	if labels[ownerNameLabel] != "owner-ss" || labels[ownerNsLabel] != "owner-ns" {
		t.Fatalf("configure merged missing owner labels: %v", labels)
	}
	_ = c
}

// TestDiff_ConfigureDoesNotMutateLive proves a configure-diff is a pure dry-run:
// the live object's data is unchanged after the diff.
func TestDiff_ConfigureDoesNotMutateLive(t *testing.T) {
	if testCfg == nil {
		t.Skip("envtest assets unavailable")
	}
	a, c := applierFor(t)
	ctx := context.Background()

	if _, err := a.Apply(ctx, "ss", "default",
		[]*unstructured.Unstructured{configMap("cfg-no-mutate", map[string]any{"k": "live"})}, apply.ConflictHandling{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	entries, err := a.Diff(ctx, "ss", "default",
		[]*unstructured.Unstructured{configMap("cfg-no-mutate", map[string]any{"k": "changed"})}, apply.ConflictHandling{})
	if err != nil {
		t.Fatalf("Diff(configure): %v", err)
	}
	if entries[0].Action != apply.DiffConfigure {
		t.Fatalf("want configure, got %s", entries[0].Action)
	}

	var live unstructured.Unstructured
	live.SetGroupVersionKind(configMap("cfg-no-mutate", nil).GroupVersionKind())
	if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "cfg-no-mutate"}, &live); err != nil {
		t.Fatalf("Get: %v", err)
	}
	got, _, _ := unstructured.NestedString(live.Object, "data", "k")
	if got != "live" {
		t.Fatalf("dry-run Diff mutated live object: data.k = %q, want %q", got, "live")
	}
}

// TestDiff_MultipleObjectsInOrder proves a single Diff call over a mixed slice
// returns one entry per object, in input order, with the correct per-object action
// and the correct nil-ness of Existing/Merged for each action.
func TestDiff_MultipleObjectsInOrder(t *testing.T) {
	if testCfg == nil {
		t.Skip("envtest assets unavailable")
	}
	a, _ := applierFor(t)
	ctx := context.Background()

	// Pre-seed two objects: one we will leave unchanged, one we will change.
	if _, err := a.Apply(ctx, "ss", "default", []*unstructured.Unstructured{
		configMap("cfg-multi-unchanged", map[string]any{"k": "same"}),
		configMap("cfg-multi-configure", map[string]any{"k": "old"}),
	}, apply.ConflictHandling{}); err != nil {
		t.Fatalf("Apply seed: %v", err)
	}

	objs := []*unstructured.Unstructured{
		configMap("cfg-multi-create", map[string]any{"k": "new"}),     // create
		configMap("cfg-multi-unchanged", map[string]any{"k": "same"}), // unchanged
		configMap("cfg-multi-configure", map[string]any{"k": "new"}),  // configure
	}
	entries, err := a.Diff(ctx, "ss", "default", objs, apply.ConflictHandling{})
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("want 3 entries, got %d", len(entries))
	}

	wantOrder := []struct {
		name   string
		action apply.DiffAction
	}{
		{"cfg-multi-create", apply.DiffCreate},
		{"cfg-multi-unchanged", apply.DiffUnchanged},
		{"cfg-multi-configure", apply.DiffConfigure},
	}
	for i, w := range wantOrder {
		e := entries[i]
		if e.Name != w.name {
			t.Fatalf("entry %d: name = %q, want %q (order not preserved)", i, e.Name, w.name)
		}
		if e.Action != w.action {
			t.Fatalf("entry %d (%s): action = %s, want %s", i, e.Name, e.Action, w.action)
		}
		// Every entry must populate GVK/Namespace/Name.
		if e.GVK.Kind != "ConfigMap" || e.Namespace != "default" {
			t.Fatalf("entry %d: GVK/Namespace not populated: %+v", i, e)
		}
		switch w.action {
		case apply.DiffCreate:
			if e.Existing != nil || e.Merged == nil {
				t.Fatalf("create entry: want Existing nil + Merged set, got existing=%v merged=%v", e.Existing, e.Merged)
			}
		case apply.DiffConfigure:
			if e.Existing == nil || e.Merged == nil {
				t.Fatalf("configure entry: want both set, got existing=%v merged=%v", e.Existing, e.Merged)
			}
		case apply.DiffUnchanged:
			if e.Existing != nil || e.Merged != nil {
				t.Fatalf("unchanged entry: want both nil, got existing=%v merged=%v", e.Existing, e.Merged)
			}
		}
	}
}

// TestDiff_ClusterScopedNamespace proves a cluster-scoped object (Namespace)
// diffs create then unchanged correctly. Its merged entry has no namespace but a
// populated name and GVK.
func TestDiff_ClusterScopedNamespace(t *testing.T) {
	if testCfg == nil {
		t.Skip("envtest assets unavailable")
	}
	a, c := applierFor(t)
	ctx := context.Background()

	ns := "ss-cluster-scoped-ns"
	entries, err := a.Diff(ctx, "ss", "default", []*unstructured.Unstructured{namespaceObj(ns)}, apply.ConflictHandling{})
	if err != nil {
		t.Fatalf("Diff(create): %v", err)
	}
	if entries[0].Action != apply.DiffCreate {
		t.Fatalf("want create, got %s", entries[0].Action)
	}
	if entries[0].Namespace != "" || entries[0].Name != ns || entries[0].GVK.Kind != "Namespace" {
		t.Fatalf("cluster-scoped entry fields wrong: %+v", entries[0])
	}
	// Dry-run must not persist.
	var probe unstructured.Unstructured
	probe.SetGroupVersionKind(namespaceObj(ns).GroupVersionKind())
	if err := c.Get(ctx, client.ObjectKey{Name: ns}, &probe); err == nil {
		t.Fatal("dry-run Diff created the Namespace")
	}

	if _, err := a.Apply(ctx, "ss", "default", []*unstructured.Unstructured{namespaceObj(ns)}, apply.ConflictHandling{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	entries, err = a.Diff(ctx, "ss", "default", []*unstructured.Unstructured{namespaceObj(ns)}, apply.ConflictHandling{})
	if err != nil {
		t.Fatalf("Diff(unchanged): %v", err)
	}
	if entries[0].Action != apply.DiffUnchanged {
		t.Fatalf("want unchanged, got %s", entries[0].Action)
	}
}

// TestDiff_Idempotent proves diffing the same applied object twice yields the same
// DiffUnchanged result, with no hidden state carried between calls.
func TestDiff_Idempotent(t *testing.T) {
	if testCfg == nil {
		t.Skip("envtest assets unavailable")
	}
	a, _ := applierFor(t)
	ctx := context.Background()

	if _, err := a.Apply(ctx, "ss", "default",
		[]*unstructured.Unstructured{configMap("cfg-idem", map[string]any{"k": "v"})}, apply.ConflictHandling{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	for i := 0; i < 3; i++ {
		entries, err := a.Diff(ctx, "ss", "default",
			[]*unstructured.Unstructured{configMap("cfg-idem", map[string]any{"k": "v"})}, apply.ConflictHandling{})
		if err != nil {
			t.Fatalf("Diff #%d: %v", i, err)
		}
		if entries[0].Action != apply.DiffUnchanged {
			t.Fatalf("Diff #%d: want unchanged, got %s", i, entries[0].Action)
		}
		if entries[0].Existing != nil || entries[0].Merged != nil {
			t.Fatalf("Diff #%d: unchanged should carry no objects, got %+v", i, entries[0])
		}
	}
}

// TestDiff_SecretDataPreserved proves Diff itself does not drop or mangle Secret
// data: the merged preview carries the (base64-encoded) data for create and
// configure. Masking, if any, is a higher layer's concern.
func TestDiff_SecretDataPreserved(t *testing.T) {
	if testCfg == nil {
		t.Skip("envtest assets unavailable")
	}
	a, _ := applierFor(t)
	ctx := context.Background()

	// create
	entries, err := a.Diff(ctx, "ss", "default",
		[]*unstructured.Unstructured{secret("sec-create", map[string]any{"token": "abc123"})}, apply.ConflictHandling{})
	if err != nil {
		t.Fatalf("Diff(create secret): %v", err)
	}
	if entries[0].Action != apply.DiffCreate || entries[0].Merged == nil {
		t.Fatalf("want create with merged, got %+v", entries[0])
	}
	// stringData survives into the merged object (the dry-run object is the desired
	// object pre-server-conversion for a create).
	sd, found, _ := unstructured.NestedStringMap(entries[0].Merged.Object, "stringData")
	if !found || sd["token"] != "abc123" {
		t.Fatalf("create merged secret dropped data: stringData=%v", sd)
	}

	// configure: apply, then change the value.
	if _, err := a.Apply(ctx, "ss", "default",
		[]*unstructured.Unstructured{secret("sec-cfg", map[string]any{"token": "old"})}, apply.ConflictHandling{}); err != nil {
		t.Fatalf("Apply secret: %v", err)
	}
	entries, err = a.Diff(ctx, "ss", "default",
		[]*unstructured.Unstructured{secret("sec-cfg", map[string]any{"token": "new"})}, apply.ConflictHandling{})
	if err != nil {
		t.Fatalf("Diff(configure secret): %v", err)
	}
	if entries[0].Action != apply.DiffConfigure || entries[0].Merged == nil || entries[0].Existing == nil {
		t.Fatalf("want configure with both objects, got %+v", entries[0])
	}
	// The merged secret must still carry a data field (its value may be masked by
	// the underlying ssa layer, but the field itself is not dropped).
	if _, found, _ := unstructured.NestedMap(entries[0].Merged.Object, "data"); !found {
		t.Fatalf("configure merged secret has no data field: %v", entries[0].Merged.Object)
	}
}

// TestDiff_ForceSelectorRecreatesImmutable proves the ForceSelector path: an
// immutable Secret whose data changes yields an error under plain apply, but a
// DiffCreate prediction (delete+recreate) when a matching ForceSelector is set.
func TestDiff_ForceSelectorRecreatesImmutable(t *testing.T) {
	if testCfg == nil {
		t.Skip("envtest assets unavailable")
	}
	a, _ := applierFor(t)
	ctx := context.Background()

	immutable := func(name string, val string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1", "kind": "Secret",
			"metadata": map[string]any{
				"name":      name,
				"namespace": "default",
				"annotations": map[string]any{
					"stages.metio.wtf/force": "enabled",
				},
			},
			"immutable":  true,
			"stringData": map[string]any{"token": val},
		}}
	}

	name := "sec-immutable"
	if _, err := a.Apply(ctx, "ss", "default", []*unstructured.Unstructured{immutable(name, "v1")}, apply.ConflictHandling{}); err != nil {
		t.Fatalf("Apply immutable secret: %v", err)
	}

	// Without ForceSelector: changing the immutable data is an error surfaced by Diff.
	if _, err := a.Diff(ctx, "ss", "default", []*unstructured.Unstructured{immutable(name, "v2")}, apply.ConflictHandling{}); err == nil {
		t.Fatal("expected immutable-field error from Diff without ForceSelector, got nil")
	}

	// With a matching ForceSelector: Diff predicts a recreate (DiffCreate).
	entries, err := a.Diff(ctx, "ss", "default",
		[]*unstructured.Unstructured{immutable(name, "v2")},
		apply.ConflictHandling{ForceSelector: map[string]string{"stages.metio.wtf/force": "enabled"}})
	if err != nil {
		t.Fatalf("Diff with ForceSelector: %v", err)
	}
	if entries[0].Action != apply.DiffCreate {
		t.Fatalf("ForceSelector should predict recreate (DiffCreate), got %s", entries[0].Action)
	}
}

// TestDiff_IfNotPresentSelectorSkips proves the DiffSkipped path: an existing
// object carrying a label matching IfNotPresentSelector is excluded from the
// dry-run apply even though its data drifted.
func TestDiff_IfNotPresentSelectorSkips(t *testing.T) {
	if testCfg == nil {
		t.Skip("envtest assets unavailable")
	}
	a, _ := applierFor(t)
	ctx := context.Background()

	labeled := func(name, val string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{
				"name":      name,
				"namespace": "default",
				"annotations": map[string]any{
					"stages.metio.wtf/keep": "yes",
				},
			},
			"data": map[string]any{"k": val},
		}}
	}

	name := "cfg-keep"
	if _, err := a.Apply(ctx, "ss", "default", []*unstructured.Unstructured{labeled(name, "original")}, apply.ConflictHandling{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Drift the data, but mark it keep-existing: Diff must report DiffSkipped.
	entries, err := a.Diff(ctx, "ss", "default",
		[]*unstructured.Unstructured{labeled(name, "drifted")},
		apply.ConflictHandling{IfNotPresentSelector: map[string]string{"stages.metio.wtf/keep": "yes"}})
	if err != nil {
		t.Fatalf("Diff with IfNotPresentSelector: %v", err)
	}
	if entries[0].Action != apply.DiffSkipped {
		t.Fatalf("IfNotPresentSelector on existing object should skip, got %s", entries[0].Action)
	}
	// Skipped entries carry no preview objects.
	if entries[0].Existing != nil || entries[0].Merged != nil {
		t.Fatalf("skipped entry should carry no objects, got %+v", entries[0])
	}
}

// TestDiff_MalformedObjectReturnsError proves an unmappable object (bogus
// apiVersion/kind) surfaces an error from Diff rather than being swallowed.
func TestDiff_MalformedObjectReturnsError(t *testing.T) {
	if testCfg == nil {
		t.Skip("envtest assets unavailable")
	}
	a, _ := applierFor(t)
	ctx := context.Background()

	bogus := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "nope.example.com/v9",
		"kind":       "DefinitelyNotAKind",
		"metadata":   map[string]any{"name": "bogus", "namespace": "default"},
	}}

	if _, err := a.Diff(ctx, "ss", "default", []*unstructured.Unstructured{bogus}, apply.ConflictHandling{}); err == nil {
		t.Fatal("expected error from Diff for unmappable object, got nil")
	}
}
