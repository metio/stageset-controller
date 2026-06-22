// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package build

import (
	"strings"
	"testing"

	apiskustomize "github.com/fluxcd/pkg/apis/kustomize"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func newNamespace(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata":   map[string]any{"name": name},
	}}
}

func newConfigMap(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]any{"name": name},
	}}
}

// An empty input set forces the Flux generator to inject its placeholder
// Namespace so kustomize has something to build; Build must drop it and return
// no objects.
func TestBuild_EmptyInputDropsPlaceholder(t *testing.T) {
	t.Parallel()
	objs, err := Build(map[string]string{}, Options{}, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(objs) != 0 {
		t.Fatalf("placeholder namespace must be dropped, got %d objects", len(objs))
	}
}

// A real Namespace whose name is not the placeholder sentinel survives the
// drop step.
func TestBuild_RealNamespaceSurvives(t *testing.T) {
	t.Parallel()
	ns := `apiVersion: v1
kind: Namespace
metadata:
  name: team-a
`
	objs, err := Build(map[string]string{"ns.yaml": ns}, Options{}, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	byKindName(t, objs, "Namespace", "team-a")
}

// A "." path is normalized to the artifact root rather than treated as a
// subdirectory.
func TestBuild_DotPathBuildsRoot(t *testing.T) {
	t.Parallel()
	objs, err := Build(map[string]string{"cm.yaml": configMap}, Options{Path: "."}, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	byKindName(t, objs, "ConfigMap", "app-config")
}

// Surrounding slashes on the path are trimmed before resolution.
func TestBuild_PathSlashesTrimmed(t *testing.T) {
	t.Parallel()
	files := map[string]string{"base/cm.yaml": configMap}
	objs, err := Build(files, Options{Path: "/base/"}, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(objs) != 1 {
		t.Fatalf("want 1 object from base/, got %d", len(objs))
	}
	byKindName(t, objs, "ConfigMap", "app-config")
}

// A path pointing at a file (not a directory) is rejected: the stat succeeds
// but IsDir() is false.
func TestBuild_PathIsFileFails(t *testing.T) {
	t.Parallel()
	files := map[string]string{"cm.yaml": configMap}
	_, err := Build(files, Options{Path: "cm.yaml"}, nil)
	if err == nil {
		t.Fatal("expected error for a path that names a file")
	}
	if !strings.Contains(err.Error(), "not found in artifact") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// A path that escapes the artifact root via ".." is rejected before any build.
func TestBuild_PathEscapeFails(t *testing.T) {
	t.Parallel()
	_, err := Build(map[string]string{"cm.yaml": configMap}, Options{Path: "../../etc"}, nil)
	if err == nil {
		t.Fatal("expected error for an escaping build path")
	}
	if !strings.Contains(err.Error(), "escapes the artifact root") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// An absolute artifact entry path is refused by materialize.
func TestBuild_AbsoluteEntryFails(t *testing.T) {
	t.Parallel()
	_, err := Build(map[string]string{"/etc/passwd": configMap}, Options{}, nil)
	if err == nil {
		t.Fatal("expected error for an absolute artifact entry")
	}
	if !strings.Contains(err.Error(), "unsafe artifact entry") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// A traversal artifact entry path is refused by materialize.
func TestBuild_TraversalEntryFails(t *testing.T) {
	t.Parallel()
	_, err := Build(map[string]string{"../escape.yaml": configMap}, Options{}, nil)
	if err == nil {
		t.Fatal("expected error for a traversal artifact entry")
	}
	if !strings.Contains(err.Error(), "unsafe artifact entry") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// Invalid YAML in the artifact fails the kustomize build step.
func TestBuild_InvalidYAMLFails(t *testing.T) {
	t.Parallel()
	_, err := Build(map[string]string{"bad.yaml": "this: is: not: valid: yaml\n  - broken"}, Options{}, nil)
	if err == nil {
		t.Fatal("expected error for invalid YAML")
	}
}

// envsubst runs in strict mode: a ${var} the output references that vars does
// not define fails the post-build substitution step.
func TestBuild_SubstitutionUndefinedVarFails(t *testing.T) {
	t.Parallel()
	cm := `apiVersion: v1
kind: ConfigMap
metadata:
  name: app-config
data:
  cluster: "${missing}"
`
	_, err := Build(map[string]string{"cm.yaml": cm}, Options{}, map[string]string{"unrelated": "x"})
	if err == nil {
		t.Fatal("expected error for an undefined substitution variable")
	}
	if !strings.Contains(err.Error(), "post-build substitution") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// An empty vars map skips substitution entirely, leaving ${...} literals
// untouched in the rendered output.
func TestBuild_EmptyVarsSkipsSubstitution(t *testing.T) {
	t.Parallel()
	cm := `apiVersion: v1
kind: ConfigMap
metadata:
  name: app-config
data:
  raw: ${literal}
`
	objs, err := Build(map[string]string{"cm.yaml": cm}, Options{}, map[string]string{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	got := byKindName(t, objs, "ConfigMap", "app-config")
	v, _, _ := unstructured.NestedString(got.Object, "data", "raw")
	if v != "${literal}" {
		t.Fatalf("substitution should be skipped, got %q", v)
	}
}

// Patches combined with a substitution variable exercise both the patch
// encoding path and post-build substitution in one build.
func TestBuild_PatchesAndSubstitution(t *testing.T) {
	t.Parallel()
	cm := `apiVersion: v1
kind: ConfigMap
metadata:
  name: app-config
data:
  cluster: ${cluster_name}
`
	patch := apiskustomize.Patch{
		Patch:  "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: app-config\ndata:\n  added: yes\n",
		Target: &apiskustomize.Selector{Kind: "ConfigMap", Name: "app-config"},
	}
	objs, err := Build(
		map[string]string{"cm.yaml": cm},
		Options{Patches: []apiskustomize.Patch{patch}},
		map[string]string{"cluster_name": "prod-fra-1"},
	)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	got := byKindName(t, objs, "ConfigMap", "app-config")
	if v, _, _ := unstructured.NestedString(got.Object, "data", "cluster"); v != "prod-fra-1" {
		t.Fatalf("substituted value = %q, want prod-fra-1", v)
	}
	if v, _, _ := unstructured.NestedString(got.Object, "data", "added"); v != "yes" {
		t.Fatalf("patched value = %q, want yes", v)
	}
}

// fluxKustomization with no patches yields an empty spec (no spec.patches key).
func TestFluxKustomization_NoPatches(t *testing.T) {
	t.Parallel()
	u, err := fluxKustomization(Options{})
	if err != nil {
		t.Fatalf("fluxKustomization: %v", err)
	}
	spec, ok := u.Object["spec"].(map[string]any)
	if !ok {
		t.Fatalf("spec missing or wrong type: %#v", u.Object["spec"])
	}
	if _, present := spec["patches"]; present {
		t.Fatalf("spec.patches must be absent when no patches are set")
	}
}

// fluxKustomization with patches populates spec.patches as a decoded slice.
func TestFluxKustomization_WithPatches(t *testing.T) {
	t.Parallel()
	patch := apiskustomize.Patch{
		Patch:  "kind: ConfigMap\nmetadata:\n  name: x\n",
		Target: &apiskustomize.Selector{Kind: "ConfigMap", Name: "x"},
	}
	u, err := fluxKustomization(Options{Patches: []apiskustomize.Patch{patch}})
	if err != nil {
		t.Fatalf("fluxKustomization: %v", err)
	}
	spec := u.Object["spec"].(map[string]any)
	patches, ok := spec["patches"].([]any)
	if !ok {
		t.Fatalf("spec.patches has wrong type: %#v", spec["patches"])
	}
	if len(patches) != 1 {
		t.Fatalf("want 1 patch, got %d", len(patches))
	}
}

// dropPlaceholder removes only the sentinel Namespace, preserving other
// objects and a real Namespace.
func TestDropPlaceholder_FiltersOnlySentinel(t *testing.T) {
	t.Parallel()
	placeholder := newNamespace(placeholderNamespace)
	real := newNamespace("keepme")
	cm := newConfigMap("cfg")
	out := dropPlaceholder([]*unstructured.Unstructured{placeholder, real, cm})
	if len(out) != 2 {
		t.Fatalf("want 2 objects after drop, got %d", len(out))
	}
	for _, o := range out {
		if o.GetKind() == "Namespace" && o.GetName() == placeholderNamespace {
			t.Fatal("placeholder namespace must be dropped")
		}
	}
}

// dropPlaceholder on a slice with no placeholder returns everything unchanged.
func TestDropPlaceholder_NoPlaceholder(t *testing.T) {
	t.Parallel()
	objs := []*unstructured.Unstructured{newConfigMap("a"), newConfigMap("b")}
	out := dropPlaceholder(objs)
	if len(out) != 2 {
		t.Fatalf("want 2 objects, got %d", len(out))
	}
}
