// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package cli

import (
	"strings"
	"testing"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

// TestGet_AllNamespaces lists StageSets across namespaces and shows the
// NAMESPACE column.
func TestGet_AllNamespaces(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	nsA := makeNamespace(t, c, "getall-a")
	nsB := makeNamespace(t, c, "getall-b")
	makeStageSet(t, c, nsA, "app-a")
	makeStageSet(t, c, nsB, "app-b")

	stdout, stderr, code := runCLI(t, cfg, "get", "-A")
	if code != exitOK {
		t.Fatalf("get -A exit = %d (stderr=%s)", code, stderr)
	}
	if !strings.Contains(stdout, "NAMESPACE") {
		t.Errorf("get -A missing NAMESPACE column:\n%s", stdout)
	}
	if !strings.Contains(stdout, "app-a") || !strings.Contains(stdout, "app-b") {
		t.Errorf("get -A missing StageSets from both namespaces:\n%s", stdout)
	}
	if !strings.Contains(stdout, nsA) || !strings.Contains(stdout, nsB) {
		t.Errorf("get -A missing namespace names:\n%s", stdout)
	}
}

// TestGet_JSONOutput renders a single StageSet as JSON with the StageSet kind
// stamped.
func TestGet_JSONOutput(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "getjson")
	makeStageSet(t, c, ns, "jsonapp")

	stdout, stderr, code := runCLI(t, cfg, "get", "jsonapp", "-n", ns, "-o", "json")
	if code != exitOK {
		t.Fatalf("get -o json exit = %d (stderr=%s)", code, stderr)
	}
	if !strings.Contains(stdout, `"kind": "StageSet"`) {
		t.Errorf("json output missing kind:\n%s", stdout)
	}
	if !strings.Contains(stdout, `"name": "jsonapp"`) {
		t.Errorf("json output missing name:\n%s", stdout)
	}
}

// TestGet_JSONListOutput renders the list form as JSON with the list kind.
func TestGet_JSONListOutput(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "getjsonlist")
	makeStageSet(t, c, ns, "one")

	stdout, _, code := runCLI(t, cfg, "get", "-n", ns, "-o", "json")
	if code != exitOK {
		t.Fatalf("get list -o json exit = %d", code)
	}
	if !strings.Contains(stdout, `"kind": "StageSetList"`) {
		t.Errorf("json list output missing list kind:\n%s", stdout)
	}
}

// TestDiff_ClientSide_Create exercises the --server-side=false path
// (clientSideChanges) against an object that does not yet exist.
func TestDiff_ClientSide_Create(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "csdcreate")
	makeStageSet(t, c, ns, "app")
	dir := writeSourceTree(t, map[string]string{
		"cm.yaml": configMapManifest(ns, "settings", map[string]string{"greeting": "hello"}),
	})

	stdout, stderr, code := runCLI(t, cfg, "diff", "app", "-n", ns,
		"--source-dir", dir, "--color", "never", "--server-side=false")
	if code != exitDiff {
		t.Fatalf("client-side diff exit = %d, want %d (stderr=%s)\n%s", code, exitDiff, stderr, stdout)
	}
	if !strings.Contains(stdout, "create ConfigMap/settings") {
		t.Errorf("client-side create not shown:\n%s", stdout)
	}
}

// TestDiff_ClientSide_Configure exercises clientSideChanges against a live
// object whose render differs.
func TestDiff_ClientSide_Configure(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "csdconfig")
	makeStageSet(t, c, ns, "app")
	createConfigMap(t, c, ns, "settings", map[string]any{"greeting": "old"})

	dir := writeSourceTree(t, map[string]string{
		"cm.yaml": configMapManifest(ns, "settings", map[string]string{"greeting": "new"}),
	})

	stdout, _, code := runCLI(t, cfg, "diff", "app", "-n", ns,
		"--source-dir", dir, "--color", "never", "--server-side=false")
	if code != exitDiff {
		t.Fatalf("client-side configure exit = %d, want %d\n%s", code, exitDiff, stdout)
	}
	if !strings.Contains(stdout, "configure ConfigMap/settings") {
		t.Errorf("client-side configure not shown:\n%s", stdout)
	}
}

// TestDiff_ClientSide_Unchanged exercises the equalRender path: a render that
// matches the live object is reported clean (exit 0, no per-object change).
func TestDiff_ClientSide_Unchanged(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "csdsame")
	makeStageSet(t, c, ns, "app")
	createConfigMapWithStageLabel(t, c, ns, "settings", "first", map[string]any{"greeting": "same"})

	dir := writeSourceTree(t, map[string]string{
		"cm.yaml": configMapManifest(ns, "settings", map[string]string{"greeting": "same"}),
	})

	stdout, stderr, code := runCLI(t, cfg, "diff", "app", "-n", ns,
		"--source-dir", dir, "--color", "never", "--server-side=false")
	if code != exitOK {
		t.Fatalf("client-side unchanged exit = %d, want %d (stderr=%s)\n%s", code, exitOK, stderr, stdout)
	}
	if strings.Contains(stdout, "configure") || strings.Contains(stdout, "create") {
		t.Errorf("identical render should be unchanged:\n%s", stdout)
	}
}

// TestDiff_ClientSide_ShowUnchanged surfaces the unchanged object when the flag
// is set.
func TestDiff_ClientSide_ShowUnchanged(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "csdshow")
	makeStageSet(t, c, ns, "app")
	createConfigMapWithStageLabel(t, c, ns, "settings", "first", map[string]any{"greeting": "same"})

	dir := writeSourceTree(t, map[string]string{
		"cm.yaml": configMapManifest(ns, "settings", map[string]string{"greeting": "same"}),
	})

	stdout, _, code := runCLI(t, cfg, "diff", "app", "-n", ns,
		"--source-dir", dir, "--color", "never", "--server-side=false", "--show-unchanged")
	if code != exitOK {
		t.Fatalf("client-side show-unchanged exit = %d, want %d\n%s", code, exitOK, stdout)
	}
	if !strings.Contains(stdout, "unchanged") {
		t.Errorf("--show-unchanged should list the object:\n%s", stdout)
	}
}

// TestBuild_StageFilter renders only the named stage of a two-stage StageSet.
func TestBuild_StageFilter(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "buildfilter")
	ss := makeStageSet(t, c, ns, "app")
	// Append a second stage so the filter has something to exclude.
	ss.Spec.Stages = append(ss.Spec.Stages, stagesv1.Stage{
		Name:      "second",
		SourceRef: stagesv1.SourceReference{Name: "app-artifact-2"},
	})
	if err := c.Update(t.Context(), ss); err != nil {
		t.Fatalf("add second stage: %v", err)
	}

	dir := writeSourceTree(t, map[string]string{
		"cm.yaml": configMapManifest(ns, "from-first", map[string]string{"k": "v"}),
	})

	stdout, stderr, code := runCLI(t, cfg, "build", "app", "-n", ns,
		"--source-dir", dir, "--stage", "first")
	if code != exitOK {
		t.Fatalf("build --stage exit = %d (stderr=%s)", code, stderr)
	}
	if !strings.Contains(stdout, "name: from-first") {
		t.Errorf("filtered build missing first stage output:\n%s", stdout)
	}
	// Only one stage rendered, so there must be no document separator.
	if strings.Contains(stdout, "\n---\n") {
		t.Errorf("single-stage build should not emit a document separator:\n%s", stdout)
	}
}
