// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package build

import (
	"testing"

	apiskustomize "github.com/fluxcd/pkg/apis/kustomize"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const configMap = `apiVersion: v1
kind: ConfigMap
metadata:
  name: app-config
data:
  key: value
`

const deployment = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
spec:
  replicas: 1
  selector:
    matchLabels:
      app: web
  template:
    metadata:
      labels:
        app: web
    spec:
      containers:
        - name: web
          image: nginx
`

func byKindName(t *testing.T, objs []*unstructured.Unstructured, kind, name string) *unstructured.Unstructured {
	t.Helper()
	for _, o := range objs {
		if o.GetKind() == kind && o.GetName() == name {
			return o
		}
	}
	t.Fatalf("object %s/%s not found in %d objects", kind, name, len(objs))
	return nil
}

func TestBuild_PlainManifests(t *testing.T) {
	t.Parallel()
	objs, err := Build(map[string]string{"cm.yaml": configMap, "deploy.yaml": deployment}, Options{}, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(objs) != 2 {
		t.Fatalf("want 2 objects, got %d", len(objs))
	}
	cm := byKindName(t, objs, "ConfigMap", "app-config")
	if v, _, _ := unstructured.NestedString(cm.Object, "data", "key"); v != "value" {
		t.Fatalf("ConfigMap data.key = %q", v)
	}
}

func TestBuild_Subdir(t *testing.T) {
	t.Parallel()
	files := map[string]string{
		"base/cm.yaml":    configMap,
		"other/junk.yaml": deployment, // must NOT be built
	}
	objs, err := Build(files, Options{Path: "base"}, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(objs) != 1 || objs[0].GetKind() != "ConfigMap" {
		t.Fatalf("subdir build should yield only base/, got %d objects", len(objs))
	}
}

func TestBuild_ExistingKustomization(t *testing.T) {
	t.Parallel()
	files := map[string]string{
		"kustomization.yaml": "resources:\n  - cm.yaml\nnamePrefix: prod-\n",
		"cm.yaml":            configMap,
	}
	objs, err := Build(files, Options{}, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// namePrefix from the artifact's own kustomization.yaml must apply.
	byKindName(t, objs, "ConfigMap", "prod-app-config")
}

func TestBuild_Patches(t *testing.T) {
	t.Parallel()
	patch := apiskustomize.Patch{
		Patch:  "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: web\nspec:\n  replicas: 3\n",
		Target: &apiskustomize.Selector{Kind: "Deployment", Name: "web"},
	}
	objs, err := Build(map[string]string{"deploy.yaml": deployment}, Options{Patches: []apiskustomize.Patch{patch}}, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	dep := byKindName(t, objs, "Deployment", "web")
	if r, _, _ := unstructured.NestedInt64(dep.Object, "spec", "replicas"); r != 3 {
		t.Fatalf("patched replicas = %d, want 3", r)
	}
}

func TestBuild_Substitution(t *testing.T) {
	t.Parallel()
	cm := `apiVersion: v1
kind: ConfigMap
metadata:
  name: app-config
data:
  cluster: ${cluster_name}
`
	objs, err := Build(map[string]string{"cm.yaml": cm}, Options{}, map[string]string{"cluster_name": "prod-fra-1"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	got := byKindName(t, objs, "ConfigMap", "app-config")
	if v, _, _ := unstructured.NestedString(got.Object, "data", "cluster"); v != "prod-fra-1" {
		t.Fatalf("substituted value = %q, want prod-fra-1", v)
	}
}

func TestBuild_MissingPathFails(t *testing.T) {
	t.Parallel()
	if _, err := Build(map[string]string{"cm.yaml": configMap}, Options{Path: "nope"}, nil); err == nil {
		t.Fatal("expected error for non-existent build path")
	}
}
