// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package artifact

import (
	"context"
	"errors"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

func resolverScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := stagesv1.AddToScheme(s); err != nil {
		t.Fatalf("stagesv1 AddToScheme: %v", err)
	}
	gvk := externalArtifactGVK()
	s.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
	listGVK := gvk
	listGVK.Kind += "List"
	s.AddKnownTypeWithName(listGVK, &unstructured.UnstructuredList{})
	return s
}

// externalArtifactFixture builds an ExternalArtifact with an optional
// producer back-pointer, an optional Ready=True condition, and an optional
// status.artifact.
func externalArtifactFixture(ns, name string, sourceRef map[string]any, ready bool, artifact map[string]any) *unstructured.Unstructured {
	ea := newExternalArtifact()
	ea.SetNamespace(ns)
	ea.SetName(name)
	if sourceRef != nil {
		_ = unstructured.SetNestedMap(ea.Object, sourceRef, "spec", "sourceRef")
	}
	if ready {
		_ = unstructured.SetNestedSlice(ea.Object, []any{map[string]any{
			"type":   "Ready",
			"status": "True",
			"reason": "Succeeded",
		}}, "status", "conditions")
	}
	if artifact != nil {
		_ = unstructured.SetNestedMap(ea.Object, artifact, "status", "artifact")
	}
	return ea
}

func readyArtifact() map[string]any {
	return map[string]any{
		"url":      "http://source.flux-system.svc/ns/a/abc.tar.gz",
		"revision": "v1@sha256:abc",
		"digest":   "sha256:abc",
	}
}

func snippetBackPointer(name string) map[string]any {
	return map[string]any{"apiVersion": "jaas.metio.wtf/v1", "kind": "JsonnetSnippet", "name": name}
}

func buildClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(resolverScheme(t)).WithObjects(objs...).Build()
}

func TestResolve_DirectExternalArtifact(t *testing.T) {
	t.Parallel()
	c := buildClient(t, externalArtifactFixture("ns", "art1", nil, true, readyArtifact()))
	got, err := (&Resolver{}).Resolve(context.Background(), c, stagesv1.SourceReference{Name: "art1"}, "ns")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Key() != "ns/art1" || got.Digest != "sha256:abc" || got.Revision != "v1@sha256:abc" {
		t.Fatalf("unexpected resolved artifact: %#v", got)
	}
}

func TestResolve_ProducerBackPointer(t *testing.T) {
	t.Parallel()
	c := buildClient(t, externalArtifactFixture("ns", "snip-artifact", snippetBackPointer("dashboards"), true, readyArtifact()))
	ref := stagesv1.SourceReference{APIVersion: "jaas.metio.wtf/v1", Kind: "JsonnetSnippet", Name: "dashboards"}
	got, err := (&Resolver{}).Resolve(context.Background(), c, ref, "ns")
	if err != nil {
		t.Fatalf("Resolve producer: %v", err)
	}
	if got.Key() != "ns/snip-artifact" {
		t.Fatalf("producer back-pointer should resolve to the EA, got %q", got.Key())
	}
}

func TestResolve_NotReady(t *testing.T) {
	t.Parallel()
	c := buildClient(t, externalArtifactFixture("ns", "art1", nil, false, nil))
	_, err := (&Resolver{}).Resolve(context.Background(), c, stagesv1.SourceReference{Name: "art1"}, "ns")
	if !errors.Is(err, ErrSourceNotReady) {
		t.Fatalf("want ErrSourceNotReady, got %v", err)
	}
}

func TestResolve_DirectNotFound(t *testing.T) {
	t.Parallel()
	c := buildClient(t)
	_, err := (&Resolver{}).Resolve(context.Background(), c, stagesv1.SourceReference{Name: "missing"}, "ns")
	if !errors.Is(err, ErrArtifactNotFound) {
		t.Fatalf("want ErrArtifactNotFound, got %v", err)
	}
}

func TestResolve_ProducerNotFound(t *testing.T) {
	t.Parallel()
	c := buildClient(t, externalArtifactFixture("ns", "other", snippetBackPointer("someone-else"), true, readyArtifact()))
	ref := stagesv1.SourceReference{APIVersion: "jaas.metio.wtf/v1", Kind: "JsonnetSnippet", Name: "dashboards"}
	_, err := (&Resolver{}).Resolve(context.Background(), c, ref, "ns")
	if !errors.Is(err, ErrArtifactNotFound) {
		t.Fatalf("want ErrArtifactNotFound, got %v", err)
	}
}

func TestResolve_AmbiguousProducer(t *testing.T) {
	t.Parallel()
	c := buildClient(
		t,
		externalArtifactFixture("ns", "art-a", snippetBackPointer("dashboards"), true, readyArtifact()),
		externalArtifactFixture("ns", "art-b", snippetBackPointer("dashboards"), true, readyArtifact()),
	)
	ref := stagesv1.SourceReference{APIVersion: "jaas.metio.wtf/v1", Kind: "JsonnetSnippet", Name: "dashboards"}
	_, err := (&Resolver{}).Resolve(context.Background(), c, ref, "ns")
	if !errors.Is(err, ErrAmbiguousProducer) {
		t.Fatalf("want ErrAmbiguousProducer, got %v", err)
	}
}

func TestResolve_CrossNamespaceRejected(t *testing.T) {
	t.Parallel()
	c := buildClient(t, externalArtifactFixture("other", "art1", nil, true, readyArtifact()))
	ref := stagesv1.SourceReference{Name: "art1", Namespace: "other"}
	_, err := (&Resolver{NoCrossNamespace: true}).Resolve(context.Background(), c, ref, "ns")
	if !errors.Is(err, ErrCrossNamespaceForbidden) {
		t.Fatalf("want ErrCrossNamespaceForbidden, got %v", err)
	}
}

func TestResolve_ProducerVersionAgnostic(t *testing.T) {
	t.Parallel()
	// The EA back-pointer records v1; a stage ref naming v2 of the same group
	// still resolves (match is on group/kind/name, not full apiVersion).
	c := buildClient(t, externalArtifactFixture("ns", "art1", snippetBackPointer("dashboards"), true, readyArtifact()))
	ref := stagesv1.SourceReference{APIVersion: "jaas.metio.wtf/v2", Kind: "JsonnetSnippet", Name: "dashboards"}
	if _, err := (&Resolver{}).Resolve(context.Background(), c, ref, "ns"); err != nil {
		t.Fatalf("version-agnostic producer match failed: %v", err)
	}
}
