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

func makeLifetimeStageSet(t testing.TB, c client.Client, ns, name, action string) {
	t.Helper()
	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: stagesv1.StageSetSpec{
			Interval: metav1.Duration{Duration: 5 * time.Minute},
			Stages: []stagesv1.Stage{{
				Name:      "app",
				SourceRef: stagesv1.SourceReference{Name: name + "-ea"},
				Actions: &stagesv1.StageActions{Post: []stagesv1.Action{
					{Name: action, Scope: stagesv1.ScopeLifetime, HTTP: &stagesv1.HTTPAction{URL: "http://x/y"}},
				}},
			}},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
}

func makeLedgerWithCompletion(t testing.TB, c client.Client, ns, name, stage, action string, origin stagesv1.LedgerOrigin) {
	t.Helper()
	l := &stagesv1.StageLedger{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
	if origin == stagesv1.OriginBaselined {
		l.Spec.Baseline = []stagesv1.LedgerRef{{Stage: stage, Action: action}}
	}
	if err := c.Create(context.Background(), l); err != nil {
		t.Fatalf("create ledger: %v", err)
	}
	l.Status.CompletedActions = []stagesv1.LedgerCompletion{{
		Stage: stage, Action: action, Origin: origin, CompletedAt: metav1.Now(),
	}}
	if err := c.Status().Update(context.Background(), l); err != nil {
		t.Fatalf("set ledger status: %v", err)
	}
}

func getLedger(t testing.TB, c client.Client, ns, name string) *stagesv1.StageLedger {
	t.Helper()
	var l stagesv1.StageLedger
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &l); err != nil {
		t.Fatalf("get ledger: %v", err)
	}
	return &l
}

func TestBaseline_CreatesLedgerAndAssertsAction(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "baseline")
	makeLifetimeStageSet(t, c, ns, "app", "install-database")

	stdout, stderr, code := runCLI(t, cfg, "baseline", "app", "-n", ns, "--stage", "app", "--action", "install-database")
	if code != exitOK {
		t.Fatalf("baseline exit = %d (stderr=%s)", code, stderr)
	}
	if !strings.Contains(stdout, "baselined") && !strings.Contains(stdout, "Baselined") {
		t.Errorf("missing confirmation:\n%s", stdout)
	}
	l := getLedger(t, c, ns, "app")
	if len(l.Spec.Baseline) != 1 || l.Spec.Baseline[0].Action != "install-database" {
		t.Errorf("spec.baseline = %+v, want one install-database entry", l.Spec.Baseline)
	}
}

func TestBaseline_RejectsNonLifetimeAction(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "baselinerev")
	// A StageSet whose action is Revision-scoped, not Lifetime.
	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "app"},
		Spec: stagesv1.StageSetSpec{
			Interval: metav1.Duration{Duration: 5 * time.Minute},
			Stages: []stagesv1.Stage{{
				Name:      "app",
				SourceRef: stagesv1.SourceReference{Name: "ea"},
				Actions: &stagesv1.StageActions{Post: []stagesv1.Action{
					{Name: "notify", Scope: stagesv1.ScopeRevision, HTTP: &stagesv1.HTTPAction{URL: "http://x/y"}},
				}},
			}},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	if _, _, code := runCLI(t, cfg, "baseline", "app", "-n", ns, "--stage", "app", "--action", "notify"); code == exitOK {
		t.Error("baselining a non-Lifetime action should fail")
	}
}

func TestBaseline_Export(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "baselineexport")
	makeLedgerWithCompletion(t, c, ns, "app", "app", "install-database", stagesv1.OriginExecuted)

	stdout, stderr, code := runCLI(t, cfg, "baseline", "app", "-n", ns, "--export")
	if code != exitOK {
		t.Fatalf("export exit = %d (stderr=%s)", code, stderr)
	}
	for _, want := range []string{"kind: StageLedger", "baseline:", "install-database"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("export missing %q:\n%s", want, stdout)
		}
	}
}

func TestResetLedger_RemovesCompletionAndBaseline(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "resetone")
	// A Baselined completion: reset must clear both status and spec.baseline so it
	// is not re-promoted.
	makeLedgerWithCompletion(t, c, ns, "app", "app", "install-database", stagesv1.OriginBaselined)

	_, stderr, code := runCLI(t, cfg, "reset-ledger", "app", "-n", ns, "--stage", "app", "--action", "install-database")
	if code != exitOK {
		t.Fatalf("reset exit = %d (stderr=%s)", code, stderr)
	}
	l := getLedger(t, c, ns, "app")
	if l.IsCompleted("app", "install-database") {
		t.Error("completion must be removed from status")
	}
	if len(l.Spec.Baseline) != 0 {
		t.Errorf("spec.baseline must be cleared, got %+v", l.Spec.Baseline)
	}
}

func TestResetLedger_All(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "resetall")
	makeLedgerWithCompletion(t, c, ns, "app", "app", "install-database", stagesv1.OriginExecuted)

	if _, stderr, code := runCLI(t, cfg, "reset-ledger", "app", "-n", ns, "--all"); code != exitOK {
		t.Fatalf("reset --all exit = %d (stderr=%s)", code, stderr)
	}
	if got := getLedger(t, c, ns, "app").Status.CompletedActions; len(got) != 0 {
		t.Errorf("--all must clear every completion, got %+v", got)
	}
}

func TestResetLedger_RequiresSelector(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "resetnosel")
	makeLedgerWithCompletion(t, c, ns, "app", "app", "install-database", stagesv1.OriginExecuted)

	if _, _, code := runCLI(t, cfg, "reset-ledger", "app", "-n", ns); code == exitOK {
		t.Error("reset-ledger without --all or --stage/--action should fail")
	}
}
