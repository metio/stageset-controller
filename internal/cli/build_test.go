// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

func writeSourceTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestBuild_FromSourceDir(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "build")
	makeStageSet(t, c, ns, "app")

	dir := writeSourceTree(t, map[string]string{
		"cm.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: settings\ndata:\n  greeting: hello\n",
	})

	stdout, stderr, code := runCLI(t, cfg, "build", "app", "-n", ns, "--source-dir", dir)
	if code != exitOK {
		t.Fatalf("build exit = %d (stderr=%s)", code, stderr)
	}
	if !strings.Contains(stdout, "kind: ConfigMap") || !strings.Contains(stdout, "name: settings") {
		t.Errorf("build output unexpected:\n%s", stdout)
	}
}

func TestBuild_MasksSecretsByDefault(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "buildsec")
	makeStageSet(t, c, ns, "app")

	dir := writeSourceTree(t, map[string]string{
		"secret.yaml": "apiVersion: v1\nkind: Secret\nmetadata:\n  name: creds\ndata:\n  password: c3VwZXJzZWNyZXQ=\n",
	})

	stdout, _, code := runCLI(t, cfg, "build", "app", "-n", ns, "--source-dir", dir)
	if code != exitOK {
		t.Fatalf("build exit = %d", code)
	}
	if strings.Contains(stdout, "c3VwZXJzZWNyZXQ=") {
		t.Errorf("secret leaked in masked build:\n%s", stdout)
	}
	if !strings.Contains(stdout, "value not shown") {
		t.Errorf("mask placeholder missing:\n%s", stdout)
	}

	stdout, _, code = runCLI(t, cfg, "build", "app", "-n", ns, "--source-dir", dir, "--show-secrets")
	if code != exitOK {
		t.Fatalf("build --show-secrets exit = %d", code)
	}
	if !strings.Contains(stdout, "c3VwZXJzZWNyZXQ=") {
		t.Errorf("--show-secrets should reveal value:\n%s", stdout)
	}
}

func TestBuild_UnknownStageIsError(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "buildbad")
	makeStageSet(t, c, ns, "app")
	dir := writeSourceTree(t, map[string]string{"cm.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n"})

	_, stderr, code := runCLI(t, cfg, "build", "app", "-n", ns, "--source-dir", dir, "--stage", "nope")
	if code != exitError {
		t.Fatalf("unknown stage exit = %d, want %d", code, exitError)
	}
	if !strings.Contains(stderr, "not found") {
		t.Errorf("stderr missing 'not found':\n%s", stderr)
	}
}

func TestParseSourceDirs(t *testing.T) {
	got, err := parseSourceDirs([]string{"/all", "canary=/c"})
	if err != nil {
		t.Fatal(err)
	}
	if got[""] != "/all" || got["canary"] != "/c" {
		t.Fatalf("unexpected: %v", got)
	}

	if _, err := parseSourceDirs([]string{"a=/x", "a=/y"}); err == nil {
		t.Fatal("want error for duplicate stage")
	}
}

// TestBuild_NoCrossNamespaceRefsFlag pins that --no-cross-namespace-refs makes
// the preview reject a cross-namespace stage sourceRef the same way a
// controller run with that flag would — and that without the flag the resolve
// proceeds (failing later on the artifact, not on the namespace).
func TestBuild_NoCrossNamespaceRefsFlag(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "xnsbuild")

	ss := makeStageSet(t, c, ns, "app")
	ss.Spec.Stages[0].SourceRef = stagesv1.SourceReference{Name: "elsewhere-artifact", Namespace: "other-ns"}
	if err := c.Update(context.Background(), ss); err != nil {
		t.Fatalf("set cross-namespace sourceRef: %v", err)
	}

	// With the flag: rejected as cross-namespace.
	_, stderr, code := runCLI(t, cfg, "build", "app", "-n", ns, "--no-cross-namespace-refs")
	if code == exitOK {
		t.Fatalf("--no-cross-namespace-refs must reject a cross-namespace sourceRef (stderr=%s)", stderr)
	}
	if !strings.Contains(stderr, "cross-namespace") {
		t.Errorf("expected a cross-namespace rejection, got: %s", stderr)
	}

	// Without the flag: the cross-namespace ref is allowed, so the resolve
	// proceeds and fails on the (absent) artifact instead — NOT on the namespace.
	_, stderr2, code2 := runCLI(t, cfg, "build", "app", "-n", ns)
	if code2 == exitOK {
		t.Fatalf("the artifact does not exist, so build should still fail (stderr=%s)", stderr2)
	}
	if strings.Contains(stderr2, "cross-namespace") {
		t.Errorf("without the flag, the cross-namespace ref must be allowed, got: %s", stderr2)
	}
}
