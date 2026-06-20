// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package artifact

import (
	"context"
	"errors"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
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
	// Classic Flux sources consumed directly as stage sources.
	for _, kind := range []string{"GitRepository", "OCIRepository", "Bucket"} {
		k := schema.GroupVersionKind{Group: externalArtifactGroup, Version: externalArtifactVersion, Kind: kind}
		s.AddKnownTypeWithName(k, &unstructured.Unstructured{})
	}
	return s
}

// sourceFixture builds a CR of the given GVK carrying an optional sourceRef
// back-pointer, an optional Ready=True condition, and an optional
// status.artifact.
func sourceFixture(apiVersion, kind, ns, name string, sourceRef map[string]any, ready bool, artifact map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion(apiVersion)
	u.SetKind(kind)
	u.SetNamespace(ns)
	u.SetName(name)
	if sourceRef != nil {
		_ = unstructured.SetNestedMap(u.Object, sourceRef, "spec", "sourceRef")
	}
	if ready {
		_ = unstructured.SetNestedSlice(u.Object, []any{map[string]any{
			"type":   "Ready",
			"status": "True",
			"reason": "Succeeded",
		}}, "status", "conditions")
	}
	if artifact != nil {
		_ = unstructured.SetNestedMap(u.Object, artifact, "status", "artifact")
	}
	return u
}

// externalArtifactFixture builds an ExternalArtifact (the common case).
func externalArtifactFixture(ns, name string, sourceRef map[string]any, ready bool, artifact map[string]any) *unstructured.Unstructured {
	return sourceFixture(externalArtifactGroup+"/"+externalArtifactVersion, externalArtifactKind, ns, name, sourceRef, ready, artifact)
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

// TestResolve_CrossNamespaceGating tables the --no-cross-namespace-refs gate
// across both axes: a cross-namespace ref is rejected only when the flag is set,
// and a same-namespace ref is always allowed regardless of the flag.
func TestResolve_CrossNamespaceGating(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		refNS     string // sourceRef.Namespace ("" = owner's namespace)
		noCross   bool
		wantError bool
	}{
		{name: "cross-namespace allowed when flag unset", refNS: "other", noCross: false},
		{name: "cross-namespace rejected when flag set", refNS: "other", noCross: true, wantError: true},
		{name: "same-namespace allowed with flag set", refNS: "ns", noCross: true},
		{name: "default-namespace ref allowed with flag set", refNS: "", noCross: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			artNS := tc.refNS
			if artNS == "" {
				artNS = "ns"
			}
			c := buildClient(t, externalArtifactFixture(artNS, "art1", nil, true, readyArtifact()))
			ref := stagesv1.SourceReference{Name: "art1", Namespace: tc.refNS}
			_, err := (&Resolver{NoCrossNamespace: tc.noCross}).Resolve(context.Background(), c, ref, "ns")
			switch {
			case tc.wantError && !errors.Is(err, ErrCrossNamespaceForbidden):
				t.Fatalf("want ErrCrossNamespaceForbidden, got %v", err)
			case !tc.wantError && err != nil:
				t.Fatalf("ref should resolve, got %v", err)
			}
		})
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

// A classic Flux source (GitRepository / OCIRepository / Bucket) is consumed
// directly — its own status.artifact is read, no producer back-pointer needed.
func TestResolve_DirectFluxSources(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"GitRepository", "OCIRepository", "Bucket"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			src := sourceFixture("source.toolkit.fluxcd.io/v1", kind, "ns", "repo", nil, true, readyArtifact())
			c := buildClient(t, src)
			ref := stagesv1.SourceReference{APIVersion: "source.toolkit.fluxcd.io/v1", Kind: kind, Name: "repo"}
			got, err := (&Resolver{}).Resolve(context.Background(), c, ref, "ns")
			if err != nil {
				t.Fatalf("Resolve %s: %v", kind, err)
			}
			if got.Key() != "ns/repo" || got.Digest != "sha256:abc" {
				t.Fatalf("unexpected resolved artifact: %#v", got)
			}
		})
	}
}

func TestResolve_DirectFluxSource_DefaultAPIVersion(t *testing.T) {
	t.Parallel()
	// kind set, apiVersion omitted → defaults to the source-controller group.
	c := buildClient(t, sourceFixture("source.toolkit.fluxcd.io/v1", "GitRepository", "ns", "repo", nil, true, readyArtifact()))
	ref := stagesv1.SourceReference{Kind: "GitRepository", Name: "repo"}
	if _, err := (&Resolver{}).Resolve(context.Background(), c, ref, "ns"); err != nil {
		t.Fatalf("default apiVersion direct source failed: %v", err)
	}
}

func TestResolve_DirectFluxSource_NotReady(t *testing.T) {
	t.Parallel()
	c := buildClient(t, sourceFixture("source.toolkit.fluxcd.io/v1", "OCIRepository", "ns", "repo", nil, false, nil))
	ref := stagesv1.SourceReference{Kind: "OCIRepository", Name: "repo"}
	if _, err := (&Resolver{}).Resolve(context.Background(), c, ref, "ns"); !errors.Is(err, ErrSourceNotReady) {
		t.Fatalf("want ErrSourceNotReady, got %v", err)
	}
}

func TestResolve_DirectFluxSource_Missing(t *testing.T) {
	t.Parallel()
	c := buildClient(t)
	ref := stagesv1.SourceReference{Kind: "Bucket", Name: "missing"}
	if _, err := (&Resolver{}).Resolve(context.Background(), c, ref, "ns"); !errors.Is(err, ErrArtifactNotFound) {
		t.Fatalf("want ErrArtifactNotFound, got %v", err)
	}
}

func TestResolve_DirectFluxSource_ReadyButNoArtifact(t *testing.T) {
	t.Parallel()
	// Ready=True but status.artifact not yet populated.
	c := buildClient(t, sourceFixture("source.toolkit.fluxcd.io/v1", "GitRepository", "ns", "repo", nil, true, nil))
	ref := stagesv1.SourceReference{Kind: "GitRepository", Name: "repo"}
	if _, err := (&Resolver{}).Resolve(context.Background(), c, ref, "ns"); !errors.Is(err, ErrArtifactMissing) {
		t.Fatalf("want ErrArtifactMissing, got %v", err)
	}
}

// A kind that shares a name with a classic source but lives in a different group
// is NOT a direct source — it falls through to the producer back-pointer path.
func TestResolve_SameNameDifferentGroupIsProducer(t *testing.T) {
	t.Parallel()
	backPtr := map[string]any{"apiVersion": "custom.example/v1", "kind": "GitRepository", "name": "thing"}
	c := buildClient(t, externalArtifactFixture("ns", "thing-artifact", backPtr, true, readyArtifact()))
	ref := stagesv1.SourceReference{APIVersion: "custom.example/v1", Kind: "GitRepository", Name: "thing"}
	got, err := (&Resolver{}).Resolve(context.Background(), c, ref, "ns")
	if err != nil {
		t.Fatalf("custom-group GitRepository should resolve via back-pointer: %v", err)
	}
	if got.Key() != "ns/thing-artifact" {
		t.Fatalf("want the back-referencing EA, got %q", got.Key())
	}
}

func TestIsDirectSourceKind(t *testing.T) {
	t.Parallel()
	cases := []struct {
		apiVersion, kind string
		want             bool
	}{
		{"source.toolkit.fluxcd.io/v1", "GitRepository", true},
		{"source.toolkit.fluxcd.io/v1", "OCIRepository", true},
		{"source.toolkit.fluxcd.io/v1", "Bucket", true},
		{"", "GitRepository", true},                                // empty apiVersion defaults to the source group
		{"custom.example/v1", "GitRepository", false},              // wrong group → producer
		{"source.toolkit.fluxcd.io/v1", "ExternalArtifact", false}, // EA has its own direct path
		{"jaas.metio.wtf/v1", "JsonnetSnippet", false},             // a producer
	}
	for _, tc := range cases {
		got := isDirectSourceKind(stagesv1.SourceReference{APIVersion: tc.apiVersion, Kind: tc.kind})
		if got != tc.want {
			t.Errorf("isDirectSourceKind(%s, %s) = %v, want %v", tc.apiVersion, tc.kind, got, tc.want)
		}
	}
}

func TestVerifiedState(t *testing.T) {
	t.Parallel()
	mk := func(conds ...map[string]any) *unstructured.Unstructured {
		u := &unstructured.Unstructured{Object: map[string]any{}}
		if len(conds) > 0 {
			s := make([]any, len(conds))
			for i := range conds {
				s[i] = conds[i]
			}
			if err := unstructured.SetNestedSlice(u.Object, s, "status", "conditions"); err != nil {
				t.Fatal(err)
			}
		}
		return u
	}
	if v := verifiedState(mk()); v != nil {
		t.Fatal("no conditions should yield nil")
	}
	if v := verifiedState(mk(map[string]any{"type": "Ready", "status": "True"})); v != nil {
		t.Fatal("no SourceVerified condition should yield nil")
	}
	if v := verifiedState(mk(map[string]any{"type": "SourceVerified", "status": "True"})); v == nil || !*v {
		t.Fatalf("SourceVerified=True should yield true, got %v", v)
	}
	if v := verifiedState(mk(map[string]any{"type": "SourceVerified", "status": "False"})); v == nil || *v {
		t.Fatalf("SourceVerified=False should yield false, got %v", v)
	}
}
