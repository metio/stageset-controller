// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package apply_test

import (
	"context"
	"os"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/metio/stageset-controller/internal/apply"
)

var testCfg *rest.Config

func TestMain(m *testing.M) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		// Skip the whole package cleanly when envtest assets are unavailable.
		os.Exit(0)
	}
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		panic(err)
	}
	testCfg = cfg
	code := m.Run()
	_ = env.Stop()
	os.Exit(code)
}

func applierFor(t *testing.T) (*apply.Applier, client.Client) {
	t.Helper()
	httpClient, err := rest.HTTPClientFor(testCfg)
	if err != nil {
		t.Fatal(err)
	}
	mapper, err := apiutil.NewDynamicRESTMapper(testCfg, httpClient)
	if err != nil {
		t.Fatal(err)
	}
	c, err := client.New(testCfg, client.Options{Mapper: mapper})
	if err != nil {
		t.Fatal(err)
	}
	return apply.New(c, mapper, "stages.metio.wtf"), c
}

func configMap(name string, data map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": name, "namespace": "default"},
		"data":     data,
	}}
}

func TestDiff_CreateConfigureUnchanged(t *testing.T) {
	if testCfg == nil {
		t.Skip("envtest assets unavailable")
	}
	a, c := applierFor(t)
	ctx := context.Background()

	entries, err := a.Diff(ctx, "ss", "default", []*unstructured.Unstructured{configMap("cfg", map[string]any{"k": "v1"})}, apply.ConflictHandling{})
	if err != nil {
		t.Fatalf("Diff(create): %v", err)
	}
	if len(entries) != 1 || entries[0].Action != apply.DiffCreate {
		t.Fatalf("want create, got %+v", entries)
	}

	// A dry-run diff must not have persisted anything.
	var probe unstructured.Unstructured
	probe.SetGroupVersionKind(configMap("cfg", nil).GroupVersionKind())
	if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "cfg"}, &probe); err == nil {
		t.Fatal("dry-run Diff created the object")
	}

	if _, err := a.Apply(ctx, "ss", "default", []*unstructured.Unstructured{configMap("cfg", map[string]any{"k": "v1"})}, apply.ConflictHandling{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	entries, err = a.Diff(ctx, "ss", "default", []*unstructured.Unstructured{configMap("cfg", map[string]any{"k": "v1"})}, apply.ConflictHandling{})
	if err != nil {
		t.Fatalf("Diff(unchanged): %v", err)
	}
	if entries[0].Action != apply.DiffUnchanged {
		t.Fatalf("want unchanged, got %s", entries[0].Action)
	}

	entries, err = a.Diff(ctx, "ss", "default", []*unstructured.Unstructured{configMap("cfg", map[string]any{"k": "v2"})}, apply.ConflictHandling{})
	if err != nil {
		t.Fatalf("Diff(configure): %v", err)
	}
	if entries[0].Action != apply.DiffConfigure {
		t.Fatalf("want configure, got %s", entries[0].Action)
	}
	if entries[0].Existing == nil || entries[0].Merged == nil {
		t.Fatal("configure entry should carry both existing and merged objects")
	}
}
