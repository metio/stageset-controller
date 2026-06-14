// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"testing"

	"github.com/fluxcd/pkg/apis/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/artifact"
)

func watchScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := stagesv1.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	s.AddKnownTypeWithName(artifact.ExternalArtifactGVK, &unstructured.Unstructured{})
	listGVK := artifact.ExternalArtifactGVK
	listGVK.Kind += "List"
	s.AddKnownTypeWithName(listGVK, &unstructured.UnstructuredList{})
	return s
}

func ea(ns, name, snippet string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(artifact.ExternalArtifactGVK)
	u.SetNamespace(ns)
	u.SetName(name)
	if snippet != "" {
		_ = unstructured.SetNestedMap(u.Object, map[string]any{
			"apiVersion": "jaas.metio.wtf/v1", "kind": "JsonnetSnippet", "name": snippet,
		}, "spec", "sourceRef")
	}
	return u
}

func ssReferencing(ns, name string, ref stagesv1.SourceReference) *stagesv1.StageSet {
	return &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       stagesv1.StageSetSpec{Stages: []stagesv1.Stage{{Name: "s", SourceRef: ref}}},
	}
}

func TestMapExternalArtifact_Direct(t *testing.T) {
	t.Parallel()
	c := fake.NewClientBuilder().WithScheme(watchScheme(t)).
		WithObjects(ssReferencing("ns", "app", stagesv1.SourceReference{Name: "art1"})).Build()
	r := &StageSetReconciler{Client: c}
	reqs := r.mapExternalArtifact(context.Background(), ea("ns", "art1", ""))
	if len(reqs) != 1 || reqs[0].Name != "app" {
		t.Fatalf("direct map = %v, want [app]", reqs)
	}
}

func TestMapExternalArtifact_Producer(t *testing.T) {
	t.Parallel()
	c := fake.NewClientBuilder().WithScheme(watchScheme(t)).
		WithObjects(ssReferencing("ns", "app", stagesv1.SourceReference{APIVersion: "jaas.metio.wtf/v1", Kind: "JsonnetSnippet", Name: "dash"})).Build()
	r := &StageSetReconciler{Client: c}
	reqs := r.mapExternalArtifact(context.Background(), ea("ns", "dash-artifact", "dash"))
	if len(reqs) != 1 || reqs[0].Name != "app" {
		t.Fatalf("producer map = %v, want [app]", reqs)
	}
}

func TestMapExternalArtifact_NoMatch(t *testing.T) {
	t.Parallel()
	c := fake.NewClientBuilder().WithScheme(watchScheme(t)).
		WithObjects(ssReferencing("ns", "app", stagesv1.SourceReference{Name: "other"})).Build()
	r := &StageSetReconciler{Client: c}
	if reqs := r.mapExternalArtifact(context.Background(), ea("ns", "art1", "")); len(reqs) != 0 {
		t.Fatalf("unrelated artifact mapped to %v, want none", reqs)
	}
}

func TestMapStageSetDependents(t *testing.T) {
	t.Parallel()
	a := &stagesv1.StageSet{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "dep-a"}}
	b := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "dep-b"},
		Spec:       stagesv1.StageSetSpec{DependsOn: []meta.NamespacedObjectReference{{Name: "dep-a"}}},
	}
	c := fake.NewClientBuilder().WithScheme(watchScheme(t)).WithObjects(a, b).Build()
	r := &StageSetReconciler{Client: c}
	reqs := r.mapStageSetDependents(context.Background(), a)
	if len(reqs) != 1 || reqs[0].Name != "dep-b" {
		t.Fatalf("dependents of dep-a = %v, want [dep-b]", reqs)
	}
}
