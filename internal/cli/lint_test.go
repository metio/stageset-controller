// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeLadder(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "ladder.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLintMigrations_Valid(t *testing.T) {
	path := writeLadder(t, "- name: drop-legacy\n  to: \"2.0.0\"\n  from: \"1.x\"\n  stage: db-pre\n  actions:\n    - name: del\n      delete:\n        target: {apiVersion: v1, kind: ConfigMap, name: legacy}\n")
	stdout, _, code := runCLI(t, nil, "lint-migrations", path)
	if code != exitOK {
		t.Fatalf("exit = %d, want 0\n%s", code, stdout)
	}
	for _, want := range []string{"1 migration(s) valid", "drop-legacy", "1.x → 2.0.0", "before db-pre", "delete"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("output missing %q:\n%s", want, stdout)
		}
	}
}

func TestLintMigrations_Invalid(t *testing.T) {
	// A misspelled field — strict parsing rejects it.
	path := writeLadder(t, "- name: x\n  to: \"2.0.0\"\n  acions: []\n")
	stdout, _, code := runCLI(t, nil, "lint-migrations", path)
	if code != exitDiff {
		t.Fatalf("exit = %d, want %d (lint failure)\n%s", code, exitDiff, stdout)
	}
	if !strings.Contains(stdout, "✗") {
		t.Errorf("expected a failure marker in output:\n%s", stdout)
	}
}

func TestLintMigrations_SemverChecked(t *testing.T) {
	// A bad `to` is caught by the lint even though admission/CRD would not see it
	// for a sourced ladder.
	path := writeLadder(t, "- name: x\n  to: not-a-version\n  stage: s\n")
	_, _, code := runCLI(t, nil, "lint-migrations", path)
	if code != exitDiff {
		t.Fatalf("exit = %d, want %d", code, exitDiff)
	}
}

func TestLintMigrations_MissingPath(t *testing.T) {
	_, stderr, code := runCLI(t, nil, "lint-migrations", filepath.Join(t.TempDir(), "nope.yaml"))
	if code != exitError {
		t.Fatalf("exit = %d, want %d (runtime error)\nstderr=%s", code, exitError, stderr)
	}
}

func TestLintMigrations_TransitionReport(t *testing.T) {
	path := writeLadder(t, ""+
		"- name: fires\n  to: \"2.0.0\"\n  stage: s\n  actions: [{name: del, delete: {target: {apiVersion: v1, kind: ConfigMap, name: x}}}]\n"+
		"- name: out-of-range\n  to: \"5.0.0\"\n  stage: s\n"+
		"- name: from-excluded\n  to: \"2.0.0\"\n  from: \">=1.5.0\"\n  stage: s\n")
	stdout, _, code := runCLI(t, nil, "lint-migrations", path, "--from", "1.0.0", "--to", "2.0.0")
	if code != exitOK {
		t.Fatalf("exit = %d, want 0\n%s", code, stdout)
	}
	for _, want := range []string{"transition 1.0.0 → 2.0.0", "fires", "FIRES", "out-of-range", "excluded", "from-excluded"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("report missing %q:\n%s", want, stdout)
		}
	}
}

func TestLintMigrations_FromWithoutTo(t *testing.T) {
	path := writeLadder(t, "- name: a\n  to: \"2.0.0\"\n  stage: s\n")
	_, _, code := runCLI(t, nil, "lint-migrations", path, "--from", "1.0.0")
	if code != exitError {
		t.Fatalf("exit = %d, want %d (--from without --to)", code, exitError)
	}
}
