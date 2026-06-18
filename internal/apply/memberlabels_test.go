// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package apply_test

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/metio/stageset-controller/internal/apply"
)

const stageLabel = "stages.metio.wtf/stage"

func TestStampStageLabel(t *testing.T) {
	t.Parallel()
	newObj := func() *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "cm", "namespace": "ns"},
		}}
	}

	t.Run("stamps the stage name", func(t *testing.T) {
		t.Parallel()
		obj := newObj()
		apply.StampStageLabel([]*unstructured.Unstructured{obj}, stageLabel, "stage")
		if got := obj.GetLabels()[stageLabel]; got != "stage" {
			t.Fatalf("%s = %q, want %q", stageLabel, got, "stage")
		}
	})

	t.Run("preserves pre-existing labels", func(t *testing.T) {
		t.Parallel()
		obj := newObj()
		obj.SetLabels(map[string]string{"app": "demo"})
		apply.StampStageLabel([]*unstructured.Unstructured{obj}, stageLabel, "stage")
		labels := obj.GetLabels()
		if labels["app"] != "demo" {
			t.Fatalf("existing label dropped: %v", labels)
		}
		if labels[stageLabel] != "stage" {
			t.Fatalf("stage label not added alongside existing: %v", labels)
		}
	})

	t.Run("overwrites a stale stage value", func(t *testing.T) {
		t.Parallel()
		obj := newObj()
		obj.SetLabels(map[string]string{stageLabel: "old"})
		apply.StampStageLabel([]*unstructured.Unstructured{obj}, stageLabel, "new")
		if got := obj.GetLabels()[stageLabel]; got != "new" {
			t.Fatalf("%s = %q, want %q", stageLabel, got, "new")
		}
	})

	t.Run("stamps every object in the slice", func(t *testing.T) {
		t.Parallel()
		objs := []*unstructured.Unstructured{newObj(), newObj(), newObj()}
		apply.StampStageLabel(objs, stageLabel, "stage")
		for i, o := range objs {
			if o.GetLabels()[stageLabel] != "stage" {
				t.Fatalf("object %d not stamped: %v", i, o.GetLabels())
			}
		}
	})
}
