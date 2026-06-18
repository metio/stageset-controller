// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package apply_test

import (
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/metio/stageset-controller/internal/apply"
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

// FuzzStampStageLabel hardens the invariant the diff faithfulness rests on: the
// per-stage label the CLI dry-run stamps must be byte-identical to what a
// reconcile apply writes, for ANY label key / stage / pre-existing label. A drift
// here is exactly the spurious-"configure" bug this guards against.
//
// Properties, for arbitrary inputs:
//   - never panics;
//   - the stage label equals the given stage value, and any pre-existing label
//     with a different key survives untouched;
//   - idempotent (a second stamp changes nothing);
//   - deterministic (a fresh object with the same inputs gets the same labels).
func FuzzStampStageLabel(f *testing.F) {
	f.Add("stages.metio.wtf/stage", "stage", "app", "demo")
	f.Add("", "", "", "")
	f.Add("k.io/s", "first", "k.io/s", "preset") // label key collides with stamp key
	f.Add("a.b/c", "c.d", "x", "y")

	f.Fuzz(func(t *testing.T, stageLabelKey, stage, lkey, lval string) {
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
		apply.StampStageLabel([]*unstructured.Unstructured{obj}, stageLabelKey, stage)

		if got := obj.GetLabels()[stageLabelKey]; got != stage {
			t.Fatalf("stage label %q = %q, want %q", stageLabelKey, got, stage)
		}
		if lkey != "" && lkey != stageLabelKey && obj.GetLabels()[lkey] != lval {
			t.Fatalf("pre-existing label %q=%q dropped during stamp", lkey, lval)
		}

		// Idempotence: a second stamp with the same inputs changes nothing.
		first := obj.DeepCopy()
		apply.StampStageLabel([]*unstructured.Unstructured{obj}, stageLabelKey, stage)
		if !reflect.DeepEqual(first.Object, obj.Object) {
			t.Fatalf("StampStageLabel not idempotent:\nfirst:  %#v\nsecond: %#v", first.Object, obj.Object)
		}

		// Determinism: a fresh object with identical inputs lands the same labels.
		other := build()
		apply.StampStageLabel([]*unstructured.Unstructured{other}, stageLabelKey, stage)
		if !sameLabels(other.GetLabels(), obj.GetLabels()) {
			t.Fatalf("non-deterministic labels: fresh %v vs %v", other.GetLabels(), obj.GetLabels())
		}
	})
}
