// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package apply_test

import (
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/metio/stageset-controller/internal/apply"
	"github.com/metio/stageset-controller/internal/inventory"
)

// sameLabels compares two label maps treating a nil map and an empty map as
// equal — unstructured.GetLabels returns nil for a label-free object, while a
// captured copy may be an empty non-nil map.
func sameLabels(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// FuzzStampMemberLabels hardens the invariant the diff faithfulness rests on:
// the member label the CLI dry-run stamps must be byte-identical to what a
// reconcile apply writes, for ANY mode / name / group / pre-existing label.
// A drift here is exactly the spurious-"configure" bug this guards against.
//
// Properties, for arbitrary inputs:
//   - never panics;
//   - in hybrid/applyset mode the part-of label equals the documented formula
//     (recomputed independently via ApplySetID∘ShardName) and any pre-existing
//     non-part-of label survives untouched;
//   - in every other mode (entries, empty, unknown) the object's labels are
//     left exactly as they were — no part-of label is introduced;
//   - idempotent (a second stamp changes nothing);
//   - deterministic (a fresh object with the same inputs gets the same labels).
func FuzzStampMemberLabels(f *testing.F) {
	f.Add("hybrid", "set", "stage", "ns", "stages.metio.wtf", "app", "demo")
	f.Add("applyset", "", "", "", "", "", "")
	f.Add("entries", "s", "t", "n", "g", inventory.PartOfLabel, "preset")
	f.Add("", "a.b", "c.d", "e.f", "g.h", "x", "y")
	f.Add("HYBRID", "s", "t", "n", "g", "k", "v")                       // mode match is case-sensitive: no stamp
	f.Add("hybrid", "s", "t", "n", "g", inventory.PartOfLabel, "stale") // overwrite path

	f.Fuzz(func(t *testing.T, mode, stageSet, stage, namespace, group, lkey, lval string) {
		build := func() *unstructured.Unstructured {
			o := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]any{"name": "cm"},
			}}
			if lkey != "" {
				o.SetLabels(map[string]string{lkey: lval})
			}
			return o
		}

		obj := build()
		inputLabels := map[string]string{}
		for k, v := range obj.GetLabels() {
			inputLabels[k] = v
		}

		apply.StampMemberLabels([]*unstructured.Unstructured{obj}, mode, stageSet, stage, namespace, group)

		stamping := mode == "hybrid" || mode == "applyset"
		if stamping {
			want := inventory.ApplySetID(inventory.ShardName(stageSet, stage, 0), namespace, "StageInventory", group)
			if got := obj.GetLabels()[inventory.PartOfLabel]; got != want {
				t.Fatalf("part-of = %q, want %q", got, want)
			}
			if lkey != "" && lkey != inventory.PartOfLabel && obj.GetLabels()[lkey] != lval {
				t.Fatalf("pre-existing label %q=%q dropped during stamp", lkey, lval)
			}
		} else if !sameLabels(obj.GetLabels(), inputLabels) {
			t.Fatalf("mode %q must be a no-op, but labels changed: %v -> %v", mode, inputLabels, obj.GetLabels())
		}

		// Idempotence: a second stamp with the same inputs changes nothing.
		first := obj.DeepCopy()
		apply.StampMemberLabels([]*unstructured.Unstructured{obj}, mode, stageSet, stage, namespace, group)
		if !reflect.DeepEqual(first.Object, obj.Object) {
			t.Fatalf("StampMemberLabels not idempotent:\nfirst:  %#v\nsecond: %#v", first.Object, obj.Object)
		}

		// Determinism: a fresh object with identical inputs lands the same labels
		// (the once-stamped result, since stamping is idempotent).
		other := build()
		apply.StampMemberLabels([]*unstructured.Unstructured{other}, mode, stageSet, stage, namespace, group)
		if !sameLabels(other.GetLabels(), obj.GetLabels()) {
			t.Fatalf("non-deterministic labels: fresh %v vs %v", other.GetLabels(), obj.GetLabels())
		}
	})
}
