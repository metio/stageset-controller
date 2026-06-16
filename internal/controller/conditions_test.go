// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestAllReasons_HaveRunbookPages is the drift gate: every wire-stable Reason
// in AllReasons must have a matching docs/content/runbooks/<reason>.md content
// page, so a new Reason cannot ship without its remediation page.
func TestAllReasons_HaveRunbookPages(t *testing.T) {
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoRoot := filepath.Dir(filepath.Dir(filepath.Dir(here))) // …/internal/controller/<file> → repo root
	runbookDir := filepath.Join(repoRoot, "docs", "content", "runbooks")
	for _, reason := range AllReasons {
		name := strings.ToLower(reason) + ".md"
		if _, err := os.Stat(filepath.Join(runbookDir, name)); err != nil {
			t.Errorf("Reason %q has no runbook page at docs/content/runbooks/%s", reason, name)
		}
	}
}

// TestAllReasons_CoversEveryConstant guards the reverse direction: a ReasonXxx
// constant added to conditions.go but not appended to AllReasons would silently
// bypass the drift gate. Go can't reflect over consts, so we count the source
// declarations and compare.
func TestAllReasons_CoversEveryConstant(t *testing.T) {
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	src, err := os.ReadFile(filepath.Join(filepath.Dir(here), "conditions.go"))
	if err != nil {
		t.Fatalf("read conditions.go: %v", err)
	}
	var declared []string
	for _, line := range strings.Split(string(src), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "Reason") {
			continue
		}
		if eq := strings.Index(line, "="); eq >= 0 {
			declared = append(declared, strings.TrimSpace(line[:eq]))
		}
	}
	if len(declared) != len(AllReasons) {
		t.Errorf("conditions.go declares %d Reason* constants but AllReasons has %d — keep them in sync.\n  declared: %v", len(declared), len(AllReasons), declared)
	}
}

func TestDecorateMessage(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		base   string
		reason string
		want   string
	}{
		{"no base URL: unchanged", "", ReasonStageFailed, "boom"},
		{"actionable reason gets a link", "https://example/runbooks", ReasonStageFailed, "boom (runbook: https://example/runbooks/stagefailed/)"},
		{"trailing slash stripped", "https://example/runbooks/", ReasonStageFailed, "boom (runbook: https://example/runbooks/stagefailed/)"},
		{"happy reason gets no link", "https://example/runbooks", ReasonReady, "boom"},
		{"suspended gets no link", "https://example/runbooks", ReasonSuspended, "boom"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := &StageSetReconciler{RunbookBaseURL: tc.base}
			if got := r.decorateMessage(tc.reason, "boom"); got != tc.want {
				t.Fatalf("decorateMessage = %q, want %q", got, tc.want)
			}
		})
	}
}
