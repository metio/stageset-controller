// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

// planStageSet builds a one-stage StageSet with a Revision pre action and a
// Lifetime post action, so a plan exercises both scopes.
func planStageSet(t testing.TB, c client.Client, ns, name string) {
	t.Helper()
	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: stagesv1.StageSetSpec{
			Interval: metav1.Duration{Duration: 5 * time.Minute},
			Stages: []stagesv1.Stage{{
				Name:      "first",
				SourceRef: stagesv1.SourceReference{Name: name + "-ea"},
				Actions: &stagesv1.StageActions{
					Pre:  []stagesv1.Action{{Name: "check", Scope: stagesv1.ScopeRevision, Wait: &stagesv1.WaitAction{Expr: "true"}}},
					Post: []stagesv1.Action{{Name: "install-database", Scope: stagesv1.ScopeLifetime, Wait: &stagesv1.WaitAction{Expr: "true"}}},
				},
			}},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
}

func TestPlan_ShowsActionVerdicts(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "plan")
	planStageSet(t, c, ns, "app")
	// install-database is recorded complete (unanchored), so it must SKIP.
	makeLedgerWithCompletion(t, c, ns, "app", "first", "install-database", stagesv1.OriginExecuted)
	dir := writeSourceTree(t, map[string]string{"cm.yaml": configMapManifest(ns, "settings", nil)})

	stdout, stderr, code := runCLI(t, cfg, "plan", "app", "-n", ns, "--source-dir", dir)
	// A Revision action would run, so the plan is non-empty: diff-style exit 1.
	if code != exitDiff {
		t.Fatalf("plan exit = %d (stderr=%s)\n%s", code, stderr, stdout)
	}
	for _, want := range []string{"check", "WILL RUN", "install-database", "SKIP", "Lifetime"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("plan output missing %q:\n%s", want, stdout)
		}
	}
}

func TestPlan_ExitZeroWhenNothingRuns(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "planzero")
	// A stage whose only action is a completed Lifetime one: nothing would run.
	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "app"},
		Spec: stagesv1.StageSetSpec{
			Interval: metav1.Duration{Duration: 5 * time.Minute},
			Stages: []stagesv1.Stage{{
				Name:      "first",
				SourceRef: stagesv1.SourceReference{Name: "app-ea"},
				Actions: &stagesv1.StageActions{
					Post: []stagesv1.Action{{Name: "install-database", Scope: stagesv1.ScopeLifetime, Wait: &stagesv1.WaitAction{Expr: "true"}}},
				},
			}},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	makeLedgerWithCompletion(t, c, ns, "app", "first", "install-database", stagesv1.OriginExecuted)
	dir := writeSourceTree(t, map[string]string{"cm.yaml": configMapManifest(ns, "settings", nil)})

	if _, stderr, code := runCLI(t, cfg, "plan", "app", "-n", ns, "--source-dir", dir); code != exitOK {
		t.Fatalf("plan with nothing to run should exit 0, got %d (stderr=%s)", code, stderr)
	}
}

func containsSubstr(lines []string, sub string) bool {
	for _, l := range lines {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

func TestPlanGates(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

	// A Deny update window covering `now`: delivery is held.
	deny := &stagesv1.StageSet{Spec: stagesv1.StageSetSpec{
		UpdateWindows: []stagesv1.UpdateWindow{{
			Type: "Deny",
			From: &metav1.Time{Time: now.Add(-time.Hour)},
			To:   &metav1.Time{Time: now.Add(time.Hour)},
		}},
	}}
	if g := planGates(deny, nil, deny.Spec.Stages, now); !containsSubstr(g, "update window") {
		t.Errorf("a covering Deny window should HOLD: %v", g)
	}

	// A status error-budget freeze.
	budget := &stagesv1.StageSet{Status: stagesv1.StageSetStatus{
		BudgetFreeze: &stagesv1.BudgetFreeze{Remaining: "0", ResumeThreshold: "0.05"},
	}}
	if g := planGates(budget, nil, nil, now); !containsSubstr(g, "error budget") {
		t.Errorf("a status budget freeze should HOLD: %v", g)
	}

	// A stage awaiting a manual promotion.
	promo := &stagesv1.StageSet{Spec: stagesv1.StageSetSpec{Stages: []stagesv1.Stage{{Name: "app"}}}}
	prior := map[string]stagesv1.StageStatus{"app": {PromotionState: &stagesv1.PromotionState{Phase: stagesv1.PromotionAwaitingManual}}}
	if g := planGates(promo, prior, promo.Spec.Stages, now); !containsSubstr(g, "promotion (app)") {
		t.Errorf("a stage awaiting promotion should HOLD: %v", g)
	}

	// Nothing holding: no gate lines.
	if g := planGates(&stagesv1.StageSet{}, nil, nil, now); len(g) != 0 {
		t.Errorf("no gates expected, got %v", g)
	}
}

func TestPlan_ShowsPendingMigrations(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "planmig")
	planStageSet(t, c, ns, "app")
	// The controller's last reconcile queued a migration.
	ss := getStageSetCLI(t, c, ns, "app")
	ss.Status.PendingMigrations = []stagesv1.PendingMigration{{
		Name: "schema-1-1", To: "1.1.0", From: "1.0.x", Stage: "first", Actions: []string{"job"},
	}}
	if err := c.Status().Update(context.Background(), ss); err != nil {
		t.Fatalf("set pending migrations: %v", err)
	}
	dir := writeSourceTree(t, map[string]string{"cm.yaml": configMapManifest(ns, "settings", nil)})

	stdout, stderr, code := runCLI(t, cfg, "plan", "app", "-n", ns, "--source-dir", dir)
	if code != exitDiff {
		t.Fatalf("a pending migration makes the plan non-empty (exit 1); got %d (stderr=%s)\n%s", code, stderr, stdout)
	}
	for _, want := range []string{"migrations:", "schema-1-1", "before first"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("plan output missing %q:\n%s", want, stdout)
		}
	}
}

func getStageSetCLI(t testing.TB, c client.Client, ns, name string) *stagesv1.StageSet {
	t.Helper()
	var ss stagesv1.StageSet
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &ss); err != nil {
		t.Fatalf("get StageSet: %v", err)
	}
	return &ss
}
