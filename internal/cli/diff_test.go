// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package cli

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/apply"
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

// createConfigMapWithStageLabel creates a live ConfigMap already carrying the
// per-stage discovery label a reconcile would have stamped, so a faithful diff
// against an unchanged render reports clean.
func createConfigMapWithStageLabel(t *testing.T, c client.Client, ns, name, stage string, data map[string]any) {
	t.Helper()
	cm := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{
			"name": name, "namespace": ns,
			"labels": map[string]any{stagesv1.StageLabel: stage},
		},
		"data": data,
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

// mapperFor builds a RESTMapper against the shared envtest apiserver, matching
// how the CLI's own newClient wires one — needed so the test can write a live
// object through the same server-side apply path the controller uses.
func mapperFor(t testing.TB, cfg *rest.Config) meta.RESTMapper {
	t.Helper()
	httpClient, err := rest.HTTPClientFor(cfg)
	if err != nil {
		t.Fatalf("HTTPClientFor: %v", err)
	}
	mapper, err := apiutil.NewDynamicRESTMapper(cfg, httpClient)
	if err != nil {
		t.Fatalf("NewDynamicRESTMapper: %v", err)
	}
	return mapper
}

// applyAsReconcile writes obj into the cluster exactly as a reconcile would: it
// stamps the per-stage discovery label and then server-side-applies through the
// same Applier the controller uses, so the live object carries both the stage
// label and the owner labels. A faithful diff against this object reports no
// change.
func applyAsReconcile(t testing.TB, cfg *rest.Config, ss *stagesv1.StageSet, stage string, obj *unstructured.Unstructured) {
	t.Helper()
	mapper := mapperFor(t, cfg)
	c, err := client.New(cfg, client.Options{Scheme: scheme(), Mapper: mapper})
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	apply.StampStageLabel([]*unstructured.Unstructured{obj}, stagesv1.StageLabel, stage)
	applier := apply.New(c, mapper, stagesv1.GroupVersion.Group)
	if _, err := applier.Apply(context.Background(), ss.Name, ss.Namespace, []*unstructured.Unstructured{obj}, apply.ConflictHandling{}); err != nil {
		t.Fatalf("apply live object: %v", err)
	}
}

// renderObj is the ConfigMap a single-document source tree renders to, used to
// build the matching live object the diff is compared against.
func renderObj(ns, name string, data map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": name, "namespace": ns},
		"data":     data,
	}}
}

// TestDiff_CleanWhenLiveCarriesStageLabel is the headline regression: the
// controller stamps stages.metio.wtf/stage on every applied object, so the CLI's
// dry-run diff must stamp it too. With the live object carrying the exact label a
// reconcile writes, the diff is clean. Without the StampStageLabel mirror in
// runDiff the live label would read as a removed field and surface as a spurious
// configure.
func TestDiff_CleanWhenLiveCarriesStageLabel(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "diffstagelabelclean")
	ss := makeStageSet(t, c, ns, "app")

	applyAsReconcile(t, cfg, ss, "first", renderObj(ns, "settings", map[string]any{"greeting": "hello"}))

	dir := writeSourceTree(t, map[string]string{
		"cm.yaml": configMapManifest(ns, "settings", map[string]string{"greeting": "hello"}),
	})

	stdout, stderr, code := runCLI(t, cfg, "diff", "app", "-n", ns, "--source-dir", dir, "--color", "never")
	if code != exitOK {
		t.Fatalf("clean diff exit = %d, want %d (stderr=%s)\n%s", code, exitOK, stderr, stdout)
	}
	if strings.Contains(stdout, "configure") {
		t.Errorf("clean diff must not show a configure (stage-label churn):\n%s", stdout)
	}
	if !strings.Contains(stdout, "unchanged") {
		t.Errorf("clean diff should report the object as unchanged:\n%s", stdout)
	}
}

// TestDiff_MissingStageLabelShowsConfigure is the negative control: a live object
// applied out-of-band WITHOUT the stage label must surface as a configure. This
// proves the diff genuinely compares the label rather than blindly suppressing it.
func TestDiff_MissingStageLabelShowsConfigure(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "diffstagelabeldrift")
	makeStageSet(t, c, ns, "app")

	// Created directly, with no stage label and no controller field manager —
	// the dry-run apply will add the stage label, so the diff must show it.
	createConfigMap(t, c, ns, "settings", map[string]any{"greeting": "hello"})

	dir := writeSourceTree(t, map[string]string{
		"cm.yaml": configMapManifest(ns, "settings", map[string]string{"greeting": "hello"}),
	})

	stdout, stderr, code := runCLI(t, cfg, "diff", "app", "-n", ns, "--source-dir", dir, "--color", "never")
	if code != exitDiff {
		t.Fatalf("drift diff exit = %d, want %d (stderr=%s)\n%s", code, exitDiff, stderr, stdout)
	}
	if !strings.Contains(stdout, "configure ConfigMap/settings") {
		t.Errorf("missing stage label should show as configure:\n%s", stdout)
	}
	if !strings.Contains(stdout, stagesv1.StageLabel) {
		t.Errorf("configure diff should reveal the stage label being added:\n%s", stdout)
	}
}

// TestDiff_CleanServerSide confirms the default server-side path exits 0 with a
// clean summary when the live object already matches the render (entries mode,
// applied via the same Applier so owner labels match).
func TestDiff_CleanServerSide(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "diffclean")
	ss := makeStageSet(t, c, ns, "app")

	applyAsReconcile(t, cfg, ss, "first", renderObj(ns, "settings", map[string]any{"greeting": "stable"}))

	dir := writeSourceTree(t, map[string]string{
		"cm.yaml": configMapManifest(ns, "settings", map[string]string{"greeting": "stable"}),
	})

	stdout, stderr, code := runCLI(t, cfg, "diff", "app", "-n", ns, "--source-dir", dir, "--color", "never")
	if code != exitOK {
		t.Fatalf("clean server-side diff exit = %d, want %d (stderr=%s)\n%s", code, exitOK, stderr, stdout)
	}
	if strings.Contains(stdout, "configure") || strings.Contains(stdout, "create") || strings.Contains(stdout, "delete") {
		t.Errorf("clean server-side diff should report no per-object changes:\n%s", stdout)
	}
}

// TestDiff_MultiStage diffs a two-stage StageSet, mapping a distinct source dir
// per stage via the STAGE=PATH form, and asserts both stages' creates appear.
func TestDiff_MultiStage(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "diffmulti")
	ss := makeStageSet(t, c, ns, "app")
	ss.Spec.Stages = append(ss.Spec.Stages, stagesv1.Stage{
		Name:      "second",
		SourceRef: stagesv1.SourceReference{Name: "app-artifact-2"},
	})
	if err := c.Update(context.Background(), ss); err != nil {
		t.Fatalf("add second stage: %v", err)
	}

	firstDir := writeSourceTree(t, map[string]string{
		"cm.yaml": configMapManifest(ns, "from-first", map[string]string{"k": "1"}),
	})
	secondDir := writeSourceTree(t, map[string]string{
		"cm.yaml": configMapManifest(ns, "from-second", map[string]string{"k": "2"}),
	})

	stdout, stderr, code := runCLI(t, cfg, "diff", "app", "-n", ns, "--color", "never",
		"--source-dir", "first="+firstDir, "--source-dir", "second="+secondDir)
	if code != exitDiff {
		t.Fatalf("multi-stage diff exit = %d, want %d (stderr=%s)\n%s", code, exitDiff, stderr, stdout)
	}
	if !strings.Contains(stdout, "create ConfigMap/from-first") {
		t.Errorf("first stage create missing:\n%s", stdout)
	}
	if !strings.Contains(stdout, "create ConfigMap/from-second") {
		t.Errorf("second stage create missing:\n%s", stdout)
	}
	if !strings.Contains(stdout, "Summary: 2 to create") {
		t.Errorf("multi-stage summary wrong:\n%s", stdout)
	}
}

// TestDiff_StageFilter restricts the diff to one stage of a two-stage StageSet
// and asserts the excluded stage's object never appears.
func TestDiff_StageFilter(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "diffstagefilter")
	ss := makeStageSet(t, c, ns, "app")
	ss.Spec.Stages = append(ss.Spec.Stages, stagesv1.Stage{
		Name:      "second",
		SourceRef: stagesv1.SourceReference{Name: "app-artifact-2"},
	})
	if err := c.Update(context.Background(), ss); err != nil {
		t.Fatalf("add second stage: %v", err)
	}

	firstDir := writeSourceTree(t, map[string]string{
		"cm.yaml": configMapManifest(ns, "from-first", map[string]string{"k": "1"}),
	})

	stdout, stderr, code := runCLI(t, cfg, "diff", "app", "-n", ns, "--color", "never",
		"--stage", "first", "--source-dir", "first="+firstDir)
	if code != exitDiff {
		t.Fatalf("stage-filter diff exit = %d, want %d (stderr=%s)\n%s", code, exitDiff, stderr, stdout)
	}
	if !strings.Contains(stdout, "create ConfigMap/from-first") {
		t.Errorf("selected stage create missing:\n%s", stdout)
	}
	if strings.Contains(stdout, "from-second") {
		t.Errorf("excluded stage leaked into filtered diff:\n%s", stdout)
	}
}

// TestDiff_MultipleObjectsStableOrder renders several objects in one stage and
// asserts every one shows as a create, exercising the multi-object output path.
func TestDiff_MultipleObjectsStableOrder(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "diffmultiobj")
	makeStageSet(t, c, ns, "app")

	dir := writeSourceTree(t, map[string]string{
		"a.yaml": configMapManifest(ns, "alpha", map[string]string{"k": "a"}),
		"b.yaml": configMapManifest(ns, "bravo", map[string]string{"k": "b"}),
		"c.yaml": configMapManifest(ns, "charlie", map[string]string{"k": "c"}),
	})

	stdout, stderr, code := runCLI(t, cfg, "diff", "app", "-n", ns, "--source-dir", dir, "--color", "never")
	if code != exitDiff {
		t.Fatalf("multi-object diff exit = %d, want %d (stderr=%s)\n%s", code, exitDiff, stderr, stdout)
	}
	for _, name := range []string{"alpha", "bravo", "charlie"} {
		if !strings.Contains(stdout, "create ConfigMap/"+name) {
			t.Errorf("missing create for %s:\n%s", name, stdout)
		}
	}
	if !strings.Contains(stdout, "Summary: 3 to create") {
		t.Errorf("multi-object summary wrong:\n%s", stdout)
	}
}

// TestDiff_AllCreatesNoLiveObjects diffs against an empty cluster: every
// rendered object is a create and nothing is configured or deleted.
func TestDiff_AllCreatesNoLiveObjects(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "diffallcreate")
	makeStageSet(t, c, ns, "app")

	dir := writeSourceTree(t, map[string]string{
		"cm.yaml": configMapManifest(ns, "settings", map[string]string{"greeting": "fresh"}),
	})

	stdout, stderr, code := runCLI(t, cfg, "diff", "app", "-n", ns, "--source-dir", dir, "--color", "never")
	if code != exitDiff {
		t.Fatalf("all-create diff exit = %d, want %d (stderr=%s)\n%s", code, exitDiff, stderr, stdout)
	}
	if !strings.Contains(stdout, "create ConfigMap/settings") {
		t.Errorf("create missing:\n%s", stdout)
	}
	if strings.Contains(stdout, "configure") || strings.Contains(stdout, "delete") {
		t.Errorf("empty cluster diff should be create-only:\n%s", stdout)
	}
}
