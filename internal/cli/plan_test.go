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
