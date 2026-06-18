// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

// TestReconcile_WaitTimesOut drives the --wait/--timeout path end-to-end: with no
// controller to handle the freshly minted token, the poll loop must exhaust the
// timeout and report it, returning a runtime error.
func TestReconcile_WaitTimesOut(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "waitto")
	makeStageSet(t, c, ns, "app")

	_, stderr, code := runCLI(t, cfg, "reconcile", "app", "-n", ns, "--wait", "--timeout", "600ms")
	if code != exitError {
		t.Fatalf("reconcile --wait timeout exit = %d, want %d (stderr=%s)", code, exitError, stderr)
	}
	if !strings.Contains(stderr, "timed out") {
		t.Errorf("stderr should report a timeout:\n%s", stderr)
	}
}

// TestWaitForReconcile_ReturnsWhenHandled covers the success path: once the
// StageSet's status records the token as the last handled reconcile request, the
// poll loop returns nil.
func TestWaitForReconcile_ReturnsWhenHandled(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "waitok")
	ss := makeStageSet(t, c, ns, "app")

	const token = "handled-token"
	ss.Status.ReconcileRequestStatus.LastHandledReconcileAt = token
	if err := c.Status().Update(context.Background(), ss); err != nil {
		t.Fatalf("seed status: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := waitForReconcile(ctx, c, ss, reconcileOptions{timeout: 5 * time.Second}, token); err != nil {
		t.Fatalf("waitForReconcile should return nil once handled, got: %v", err)
	}
}

// TestWaitForReconcile_StageLevel covers the single-stage wait branch: the poll
// resolves against the named stage's lastHandledReconcileAt, not the top-level
// one.
func TestWaitForReconcile_StageLevel(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "waitstage")
	ss := makeStageSet(t, c, ns, "app")

	const token = "stage-token"
	ss.Status.Stages = []stagesv1.StageStatus{{Name: "first", LastHandledReconcileAt: token}}
	if err := c.Status().Update(context.Background(), ss); err != nil {
		t.Fatalf("seed stage status: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := waitForReconcile(ctx, c, ss, reconcileOptions{stage: "first", timeout: 5 * time.Second}, token); err != nil {
		t.Fatalf("waitForReconcile (stage) should return nil once handled, got: %v", err)
	}
}
