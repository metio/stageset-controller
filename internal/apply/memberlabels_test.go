// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package apply_test

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/metio/stageset-controller/internal/apply"
	"github.com/metio/stageset-controller/internal/inventory"
)

func TestStampMemberLabels(t *testing.T) {
	t.Parallel()
	newObj := func() *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "cm", "namespace": "ns"},
		}}
	}
	want := inventory.ApplySetID(inventory.ShardName("set", "stage", 0), "ns", "StageInventory", "stages.metio.wtf")

	t.Run("entries mode is a no-op", func(t *testing.T) {
		t.Parallel()
		obj := newObj()
		apply.StampMemberLabels([]*unstructured.Unstructured{obj}, "entries", "set", "stage", "ns", "stages.metio.wtf")
		if _, ok := obj.GetLabels()[inventory.PartOfLabel]; ok {
			t.Fatalf("entries mode must not stamp %s", inventory.PartOfLabel)
		}
	})

	t.Run("empty mode is a no-op", func(t *testing.T) {
		t.Parallel()
		obj := newObj()
		apply.StampMemberLabels([]*unstructured.Unstructured{obj}, "", "set", "stage", "ns", "stages.metio.wtf")
		if _, ok := obj.GetLabels()[inventory.PartOfLabel]; ok {
			t.Fatalf("empty mode must not stamp %s", inventory.PartOfLabel)
		}
	})

	for _, mode := range []string{"hybrid", "applyset"} {
		t.Run(mode+" mode stamps the shard-zero parent ID", func(t *testing.T) {
			t.Parallel()
			obj := newObj()
			apply.StampMemberLabels([]*unstructured.Unstructured{obj}, mode, "set", "stage", "ns", "stages.metio.wtf")
			if got := obj.GetLabels()[inventory.PartOfLabel]; got != want {
				t.Fatalf("%s = %q, want %q", inventory.PartOfLabel, got, want)
			}
		})
	}

	t.Run("preserves pre-existing labels", func(t *testing.T) {
		t.Parallel()
		obj := newObj()
		obj.SetLabels(map[string]string{"app": "demo"})
		apply.StampMemberLabels([]*unstructured.Unstructured{obj}, "hybrid", "set", "stage", "ns", "stages.metio.wtf")
		labels := obj.GetLabels()
		if labels["app"] != "demo" {
			t.Fatalf("existing label dropped: %v", labels)
		}
		if labels[inventory.PartOfLabel] != want {
			t.Fatalf("part-of label not added alongside existing: %v", labels)
		}
	})

	t.Run("stamps every object in the slice", func(t *testing.T) {
		t.Parallel()
		objs := []*unstructured.Unstructured{newObj(), newObj(), newObj()}
		apply.StampMemberLabels(objs, "hybrid", "set", "stage", "ns", "stages.metio.wtf")
		for i, o := range objs {
			if o.GetLabels()[inventory.PartOfLabel] != want {
				t.Fatalf("object %d not stamped: %v", i, o.GetLabels())
			}
		}
	})
}
