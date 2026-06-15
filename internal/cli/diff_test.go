// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package cli

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/inventory"
	"github.com/metio/stageset-controller/internal/stageinv"
)

func configMapManifest(ns, name string, data map[string]string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: %s\n  namespace: %s\ndata:\n", name, ns)
	for k, v := range data {
		fmt.Fprintf(&b, "  %s: %q\n", k, v)
	}
	return b.String()
}

func createConfigMap(t *testing.T, c client.Client, ns, name string, data map[string]any) {
	t.Helper()
	cm := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": name, "namespace": ns},
		"data":     data,
	}}
	if err := c.Create(context.Background(), cm); err != nil {
		t.Fatalf("create ConfigMap %s: %v", name, err)
	}
}

func TestDiff_Create(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "diffcreate")
	makeStageSet(t, c, ns, "app")
	dir := writeSourceTree(t, map[string]string{
		"cm.yaml": configMapManifest(ns, "settings", map[string]string{"greeting": "hello"}),
	})

	stdout, stderr, code := runCLI(t, cfg, "diff", "app", "-n", ns, "--source-dir", dir, "--color", "never")
	if code != exitDiff {
		t.Fatalf("diff exit = %d, want %d (stderr=%s)\n%s", code, exitDiff, stderr, stdout)
	}
	if !strings.Contains(stdout, "create ConfigMap/settings") {
		t.Errorf("missing create line:\n%s", stdout)
	}
	if !strings.Contains(stdout, "Summary: 1 to create") {
		t.Errorf("missing summary:\n%s", stdout)
	}
}

func TestDiff_Configure(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "diffconfig")
	makeStageSet(t, c, ns, "app")
	createConfigMap(t, c, ns, "settings", map[string]any{"greeting": "old"})

	dir := writeSourceTree(t, map[string]string{
		"cm.yaml": configMapManifest(ns, "settings", map[string]string{"greeting": "new"}),
	})

	stdout, _, code := runCLI(t, cfg, "diff", "app", "-n", ns, "--source-dir", dir, "--color", "never")
	if code != exitDiff {
		t.Fatalf("diff exit = %d, want %d\n%s", code, exitDiff, stdout)
	}
	if !strings.Contains(stdout, "configure ConfigMap/settings") {
		t.Errorf("missing configure line:\n%s", stdout)
	}
	if !strings.Contains(stdout, "new") {
		t.Errorf("diff body missing new value:\n%s", stdout)
	}
}

// TestDiff_ShowsPrune is the headline case: an object recorded in a stage's
// inventory that the new render no longer produces must appear as a deletion.
func TestDiff_ShowsPrune(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "diffprune")
	ss := makeStageSet(t, c, ns, "app")

	// An object the stage used to own and that still exists in the cluster.
	createConfigMap(t, c, ns, "obsolete", map[string]any{"k": "v"})
	recorder := &stageinv.Recorder{Client: c}
	if err := recorder.Write(context.Background(), ss, "first", 0, []inventory.ObjectRef{
		{Group: "", Kind: "ConfigMap", Namespace: ns, Name: "obsolete", Version: "v1"},
	}); err != nil {
		t.Fatalf("seed inventory: %v", err)
	}

	// The new render produces a different object, so "obsolete" falls out.
	dir := writeSourceTree(t, map[string]string{
		"cm.yaml": configMapManifest(ns, "settings", map[string]string{"greeting": "hi"}),
	})

	stdout, stderr, code := runCLI(t, cfg, "diff", "app", "-n", ns, "--source-dir", dir, "--color", "never")
	if code != exitDiff {
		t.Fatalf("diff exit = %d, want %d (stderr=%s)\n%s", code, exitDiff, stderr, stdout)
	}
	if !strings.Contains(stdout, "delete ConfigMap/obsolete") {
		t.Errorf("prune not shown as deletion:\n%s", stdout)
	}
	if !strings.Contains(stdout, "create ConfigMap/settings") {
		t.Errorf("create not shown:\n%s", stdout)
	}
	if !strings.Contains(stdout, "to create") || !strings.Contains(stdout, "to delete") {
		t.Errorf("summary missing create+delete:\n%s", stdout)
	}
}

func TestDiff_PruneSuppressedByFlag(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "diffnoprune")
	ss := makeStageSet(t, c, ns, "app")
	createConfigMap(t, c, ns, "obsolete", map[string]any{"k": "v"})
	recorder := &stageinv.Recorder{Client: c}
	if err := recorder.Write(context.Background(), ss, "first", 0, []inventory.ObjectRef{
		{Group: "", Kind: "ConfigMap", Namespace: ns, Name: "obsolete", Version: "v1"},
	}); err != nil {
		t.Fatalf("seed inventory: %v", err)
	}
	dir := writeSourceTree(t, map[string]string{
		"cm.yaml": configMapManifest(ns, "settings", map[string]string{"greeting": "hi"}),
	})

	stdout, _, _ := runCLI(t, cfg, "diff", "app", "-n", ns, "--source-dir", dir, "--color", "never", "--prune=false")
	if strings.Contains(stdout, "delete ConfigMap/obsolete") {
		t.Errorf("--prune=false still showed deletion:\n%s", stdout)
	}
}

func TestDiff_ExitCodeFalseAlwaysZero(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "diffexit")
	makeStageSet(t, c, ns, "app")
	dir := writeSourceTree(t, map[string]string{
		"cm.yaml": configMapManifest(ns, "settings", map[string]string{"greeting": "hello"}),
	})

	_, _, code := runCLI(t, cfg, "diff", "app", "-n", ns, "--source-dir", dir, "--color", "never", "--exit-code=false")
	if code != exitOK {
		t.Fatalf("--exit-code=false should exit 0 even with drift, got %d", code)
	}
}

func TestDiff_ShowsStageActions(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "diffact")

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "app"},
		Spec: stagesv1.StageSetSpec{
			Interval: metav1.Duration{Duration: 5 * time.Minute},
			Stages: []stagesv1.Stage{{
				Name:      "first",
				SourceRef: stagesv1.SourceReference{Name: "app-artifact"},
				Actions: &stagesv1.StageActions{
					Post: []stagesv1.Action{{Name: "smoke", HTTP: &stagesv1.HTTPAction{URL: "https://example.test/hook"}}},
				},
			}},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	dir := writeSourceTree(t, map[string]string{
		"cm.yaml": configMapManifest(ns, "settings", map[string]string{"greeting": "hi"}),
	})

	stdout, _, _ := runCLI(t, cfg, "diff", "app", "-n", ns, "--source-dir", dir, "--color", "never")
	if !strings.Contains(stdout, "Actions to run:") {
		t.Errorf("missing actions section:\n%s", stdout)
	}
	if !strings.Contains(stdout, "smoke") || !strings.Contains(stdout, "http POST https://example.test/hook") {
		t.Errorf("action detail missing:\n%s", stdout)
	}
}

func TestDiff_ShowsPendingMigrations(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "diffmig")

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "app"},
		Spec: stagesv1.StageSetSpec{
			Interval: metav1.Duration{Duration: 5 * time.Minute},
			Stages:   []stagesv1.Stage{{Name: "first", SourceRef: stagesv1.SourceReference{Name: "app-artifact"}}},
			Migrations: []stagesv1.Migration{{
				Name: "schema-upgrade", To: "v2", Stage: "first",
				Actions: []stagesv1.Action{{Name: "convert", Wait: &stagesv1.WaitAction{Expr: "true"}}},
			}},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	// The controller computes status.pendingMigrations; seed it directly.
	ss.Status.PendingMigrations = []string{"schema-upgrade"}
	if err := c.Status().Update(context.Background(), ss); err != nil {
		t.Fatalf("seed pending migrations: %v", err)
	}

	dir := writeSourceTree(t, map[string]string{
		"cm.yaml": configMapManifest(ns, "settings", map[string]string{"greeting": "hi"}),
	})
	stdout, _, _ := runCLI(t, cfg, "diff", "app", "-n", ns, "--source-dir", dir, "--color", "never")
	if !strings.Contains(stdout, "Migrations to run:") {
		t.Errorf("missing migrations section:\n%s", stdout)
	}
	if !strings.Contains(stdout, "schema-upgrade") || !strings.Contains(stdout, "→ v2") {
		t.Errorf("migration detail missing:\n%s", stdout)
	}
}

func TestDiff_MasksSecretsByDefault(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "diffsec")
	makeStageSet(t, c, ns, "app")
	dir := writeSourceTree(t, map[string]string{
		"secret.yaml": fmt.Sprintf("apiVersion: v1\nkind: Secret\nmetadata:\n  name: creds\n  namespace: %s\ndata:\n  password: c3VwZXJzZWNyZXQ=\n", ns),
	})

	stdout, _, _ := runCLI(t, cfg, "diff", "app", "-n", ns, "--source-dir", dir, "--color", "never")
	if strings.Contains(stdout, "c3VwZXJzZWNyZXQ=") {
		t.Errorf("secret leaked in diff:\n%s", stdout)
	}
	if !strings.Contains(stdout, "value not shown") {
		t.Errorf("mask placeholder missing:\n%s", stdout)
	}
}
