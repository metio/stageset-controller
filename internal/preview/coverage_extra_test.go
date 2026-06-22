// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package preview

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/artifact"
	"github.com/metio/stageset-controller/internal/inventory"
)

// TestResolvePostBuildVars_NilReturnsNothing covers the early return for a stage
// with no PostBuild block.
func TestResolvePostBuildVars_NilReturnsNothing(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	vars, err := resolvePostBuildVars(context.Background(), c, "ns", nil)
	if err != nil {
		t.Fatalf("resolvePostBuildVars(nil): %v", err)
	}
	if vars != nil {
		t.Fatalf("nil PostBuild must yield nil vars, got %v", vars)
	}
}

// TestResolvePostBuildVars_OptionalSecretMissingSkipped covers the
// optional-Secret NotFound branch: an absent optional Secret is skipped rather
// than failing the render.
func TestResolvePostBuildVars_OptionalSecretMissingSkipped(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	pb := &stagesv1.PostBuild{
		SubstituteFrom: []stagesv1.SubstituteReference{
			{Kind: "Secret", Name: "absent", Optional: true},
		},
	}
	vars, err := resolvePostBuildVars(context.Background(), c, "ns", pb)
	if err != nil {
		t.Fatalf("optional missing Secret must not error: %v", err)
	}
	if len(vars) != 0 {
		t.Fatalf("optional missing Secret must contribute nothing, got %v", vars)
	}
}

// TestResolvePostBuildVars_RequiredSecretMissingIsError covers the
// required-Secret error branch.
func TestResolvePostBuildVars_RequiredSecretMissingIsError(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	pb := &stagesv1.PostBuild{
		SubstituteFrom: []stagesv1.SubstituteReference{{Kind: "Secret", Name: "gone"}},
	}
	if _, err := resolvePostBuildVars(context.Background(), c, "ns", pb); err == nil {
		t.Fatal("want error for missing required Secret")
	}
}

// TestResolvePostBuildVars_SecretValuesDecoded confirms Secret byte values are
// folded in as strings.
func TestResolvePostBuildVars_SecretValuesDecoded(t *testing.T) {
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "creds"},
		Data:       map[string][]byte{"PASSWORD": []byte("s3cr3t")},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(sec).Build()
	pb := &stagesv1.PostBuild{
		SubstituteFrom: []stagesv1.SubstituteReference{{Kind: "Secret", Name: "creds"}},
	}
	vars, err := resolvePostBuildVars(context.Background(), c, "ns", pb)
	if err != nil {
		t.Fatalf("resolvePostBuildVars: %v", err)
	}
	if vars["PASSWORD"] != "s3cr3t" {
		t.Fatalf("Secret value not decoded, got %q", vars["PASSWORD"])
	}
}

// TestResolvePostBuildVars_UnknownKindIgnored covers the default switch arm: a
// ref whose Kind is neither ConfigMap nor Secret contributes nothing without
// erroring.
func TestResolvePostBuildVars_UnknownKindIgnored(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	pb := &stagesv1.PostBuild{
		SubstituteFrom: []stagesv1.SubstituteReference{{Kind: "Mystery", Name: "x"}},
	}
	vars, err := resolvePostBuildVars(context.Background(), c, "ns", pb)
	if err != nil {
		t.Fatalf("unknown kind must be ignored, got error: %v", err)
	}
	if len(vars) != 0 {
		t.Fatalf("unknown kind must contribute nothing, got %v", vars)
	}
}

// TestReadDirFiles_NonexistentDirErrors exercises the WalkDir error branch:
// walking a path that does not exist surfaces the walk error.
func TestReadDirFiles_NonexistentDirErrors(t *testing.T) {
	if _, err := readDirFiles(filepath.Join(t.TempDir(), "does-not-exist")); err == nil {
		t.Fatal("want error walking a nonexistent dir")
	}
}

// TestSourceDir_NilMapMisses confirms the no-SourceDirs branch reports no local
// override.
func TestSourceDir_NilMapMisses(t *testing.T) {
	e := &Engine{}
	if dir, ok := e.sourceDir("any"); ok || dir != "" {
		t.Fatalf("nil SourceDirs must report no local dir, got %q ok=%v", dir, ok)
	}
}

// TestSourceDir_ExactThenWildcard covers the named-key hit, the empty-key
// wildcard fallback, and the total miss.
func TestSourceDir_ExactThenWildcard(t *testing.T) {
	e := &Engine{SourceDirs: map[string]string{"named": "/n", "": "/wild"}}
	if dir, ok := e.sourceDir("named"); !ok || dir != "/n" {
		t.Fatalf("exact key must win, got %q ok=%v", dir, ok)
	}
	if dir, ok := e.sourceDir("other"); !ok || dir != "/wild" {
		t.Fatalf("wildcard key must apply, got %q ok=%v", dir, ok)
	}

	noWild := &Engine{SourceDirs: map[string]string{"named": "/n"}}
	if dir, ok := noWild.sourceDir("other"); ok || dir != "" {
		t.Fatalf("no wildcard and no match must miss, got %q ok=%v", dir, ok)
	}
}

// TestRenderStage_ResolveErrorWrapped drives the cluster-fetch branch of
// sourceFiles: with no SourceDir and an ExternalArtifact that does not exist,
// the resolver fails and RenderStage wraps the error naming the stage.
func TestRenderStage_ResolveErrorWrapped(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	engine := NewEngine(c, false)

	ss := stageSet("web")
	ss.Spec.Stages[0].SourceRef = stagesv1.SourceReference{Name: "missing-artifact"}

	_, err := engine.RenderStage(context.Background(), ss, &ss.Spec.Stages[0])
	if err == nil {
		t.Fatal("want error resolving a missing ExternalArtifact")
	}
}

// TestRenderStage_SourceDirReadErrorWrapped covers the local-dir error branch in
// sourceFiles: a SourceDir pointing at a nonexistent path fails readDirFiles and
// RenderStage wraps it naming the stage.
func TestRenderStage_SourceDirReadErrorWrapped(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	engine := NewEngine(c, false)
	engine.SourceDirs = map[string]string{"": filepath.Join(t.TempDir(), "absent")}

	ss := stageSet("only")
	_, err := engine.RenderStage(context.Background(), ss, &ss.Spec.Stages[0])
	if err == nil {
		t.Fatal("want error reading a nonexistent --source-dir")
	}
}

// schemeWithExternalArtifact registers the ExternalArtifact unstructured GVK on
// top of the package's base scheme so the resolver can read a seeded EA in the
// cluster-fetch path.
func schemeWithExternalArtifact(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := stagesv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	gvk := artifact.ExternalArtifactGVK
	s.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
	listGVK := gvk
	listGVK.Kind += "List"
	s.AddKnownTypeWithName(listGVK, &unstructured.UnstructuredList{})
	return s
}

// tarGz builds a deterministic gzip-compressed tarball from a path→content map.
func tarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sha256Of(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// TestRenderStage_FromClusterArtifact drives the successful cluster-fetch branch
// of sourceFiles end-to-end: a ready ExternalArtifact whose URL serves a tarball
// is resolved, fetched, and built — the render is non-local and carries the
// artifact revision.
func TestRenderStage_FromClusterArtifact(t *testing.T) {
	tarball := tarGz(t, map[string]string{"cm.yaml": `apiVersion: v1
kind: ConfigMap
metadata:
  name: settings
data:
  k: v
`})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(tarball)
	}))
	defer srv.Close()

	ea := &unstructured.Unstructured{}
	ea.SetGroupVersionKind(artifact.ExternalArtifactGVK)
	ea.SetNamespace("ns")
	ea.SetName("art")
	_ = unstructured.SetNestedSlice(ea.Object, []any{map[string]any{
		"type": "Ready", "status": "True", "reason": "Succeeded",
	}}, "status", "conditions")
	_ = unstructured.SetNestedMap(ea.Object, map[string]any{
		"url":      srv.URL + "/art.tar.gz",
		"revision": "rev-1@" + sha256Of(tarball),
		"digest":   sha256Of(tarball),
	}, "status", "artifact")

	c := fake.NewClientBuilder().WithScheme(schemeWithExternalArtifact(t)).WithObjects(ea).Build()
	engine := NewEngine(c, false)

	ss := stageSet("web")
	ss.Spec.Stages[0].SourceRef = stagesv1.SourceReference{Name: "art"}

	render, err := engine.RenderStage(context.Background(), ss, &ss.Spec.Stages[0])
	if err != nil {
		t.Fatalf("RenderStage from cluster: %v", err)
	}
	if render.Local {
		t.Error("cluster-fetched render must not be marked Local")
	}
	if render.Revision != "rev-1@"+sha256Of(tarball) {
		t.Errorf("render carries wrong revision: %q", render.Revision)
	}
	if len(render.Objects) != 1 || render.Objects[0].GetName() != "settings" {
		t.Fatalf("unexpected render objects: %v", render.Objects)
	}
}

// TestRenderStage_ClusterFetchError covers the fetch-error branch of sourceFiles:
// the EA resolves, but its artifact URL is unreachable, so Fetch fails and
// RenderStage wraps the error naming the stage.
func TestRenderStage_ClusterFetchError(t *testing.T) {
	ea := &unstructured.Unstructured{}
	ea.SetGroupVersionKind(artifact.ExternalArtifactGVK)
	ea.SetNamespace("ns")
	ea.SetName("art")
	_ = unstructured.SetNestedSlice(ea.Object, []any{map[string]any{
		"type": "Ready", "status": "True", "reason": "Succeeded",
	}}, "status", "conditions")
	_ = unstructured.SetNestedMap(ea.Object, map[string]any{
		"url":      "http://127.0.0.1:1/unreachable.tar.gz",
		"revision": "rev@sha256:00",
		"digest":   "sha256:00",
	}, "status", "artifact")

	c := fake.NewClientBuilder().WithScheme(schemeWithExternalArtifact(t)).WithObjects(ea).Build()
	engine := NewEngine(c, false)

	ss := stageSet("web")
	ss.Spec.Stages[0].SourceRef = stagesv1.SourceReference{Name: "art"}

	if _, err := engine.RenderStage(context.Background(), ss, &ss.Spec.Stages[0]); err == nil {
		t.Fatal("want error when the artifact URL is unreachable")
	}
}

// TestPrunePlan_StageRecordsErrorPropagates covers the StageRecords error branch
// of PrunePlan: a List failure from the inventory recorder bubbles up.
func TestPrunePlan_StageRecordsErrorPropagates(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(_ context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
				return apierrors.NewServiceUnavailable("inventory list down")
			},
		}).Build()
	engine := NewEngine(c, false)

	ss := stageSet("s1")
	rendered := map[string][]inventory.ObjectRef{"s1": {}}
	if _, err := engine.PrunePlan(context.Background(), ss, rendered); err == nil {
		t.Fatal("want error when StageRecords List fails")
	}
}

// TestNewEngine_PermissiveValidators pins NewEngine's wiring: the fetcher's URL
// and IP validators are permissive (the operator dials their own cluster), and
// NoCrossNamespace threads through to the resolver.
func TestNewEngine_PermissiveValidators(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	e := NewEngine(c, true)
	if e.Fetcher == nil || e.Fetcher.URLValidator == nil || e.Fetcher.IPValidator == nil {
		t.Fatal("NewEngine must install permissive URL and IP validators")
	}
	if err := e.Fetcher.URLValidator("http://anything.invalid"); err != nil {
		t.Errorf("URL validator must be permissive, got %v", err)
	}
	if err := e.Fetcher.IPValidator(nil); err != nil {
		t.Errorf("IP validator must be permissive, got %v", err)
	}
	if e.Resolver == nil || !e.Resolver.NoCrossNamespace {
		t.Error("NoCrossNamespace must thread through to the resolver")
	}
}
