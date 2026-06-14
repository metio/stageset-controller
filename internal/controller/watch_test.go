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
	"k8s.io/apimachinery/pkg/runtime/schema"
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

func producer(ns, name, apiVersion, kind string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.FromAPIVersionAndKind(apiVersion, kind))
	u.SetNamespace(ns)
	u.SetName(name)
	return u
}

// A producer object's change maps to the StageSets whose sourceRef names it,
// so a failing producer (which publishes no new artifact) still wakes its
// consumers immediately — the point of the dynamic producer watch (#7).
func TestMapProducer_Match(t *testing.T) {
	t.Parallel()
	c := fake.NewClientBuilder().WithScheme(watchScheme(t)).
		WithObjects(ssReferencing("ns", "app", stagesv1.SourceReference{APIVersion: "jaas.metio.wtf/v1", Kind: "JsonnetSnippet", Name: "dash"})).Build()
	r := &StageSetReconciler{Client: c}
	reqs := r.mapProducer(context.Background(), producer("ns", "dash", "jaas.metio.wtf/v1", "JsonnetSnippet"))
	if len(reqs) != 1 || reqs[0].Name != "app" {
		t.Fatalf("producer map = %v, want [app]", reqs)
	}
}

func TestMapProducer_NoMatch(t *testing.T) {
	t.Parallel()
	c := fake.NewClientBuilder().WithScheme(watchScheme(t)).WithObjects(
		// same kind, different name
		ssReferencing("ns", "other-name", stagesv1.SourceReference{APIVersion: "jaas.metio.wtf/v1", Kind: "JsonnetSnippet", Name: "other"}),
		// a direct-EA ref that happens to share the producer's name must NOT match
		ssReferencing("ns", "ea-app", stagesv1.SourceReference{Name: "dash"}),
	).Build()
	r := &StageSetReconciler{Client: c}
	if reqs := r.mapProducer(context.Background(), producer("ns", "dash", "jaas.metio.wtf/v1", "JsonnetSnippet")); len(reqs) != 0 {
		t.Fatalf("non-matching producer mapped to %v, want none", reqs)
	}
}

func TestProducerGVK_And_IsProducerRef(t *testing.T) {
	t.Parallel()
	if got := producerGVK(stagesv1.SourceReference{APIVersion: "jaas.metio.wtf/v1", Kind: "JsonnetSnippet"}); got != (schema.GroupVersionKind{Group: "jaas.metio.wtf", Version: "v1", Kind: "JsonnetSnippet"}) {
		t.Errorf("explicit GVK = %v", got)
	}
	if got, want := producerGVK(stagesv1.SourceReference{Kind: "GitRepository"}), artifact.ExternalArtifactGVK.GroupVersion().WithKind("GitRepository"); got != want {
		t.Errorf("default-group GVK = %v, want %v", got, want)
	}
	if isProducerRef(stagesv1.SourceReference{Name: "x"}) || isProducerRef(stagesv1.SourceReference{Kind: "ExternalArtifact", Name: "x"}) {
		t.Error("a direct ExternalArtifact ref must not be treated as a producer")
	}
	if !isProducerRef(stagesv1.SourceReference{Kind: "JsonnetSnippet", Name: "x"}) {
		t.Error("a non-EA kind must be treated as a producer")
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
