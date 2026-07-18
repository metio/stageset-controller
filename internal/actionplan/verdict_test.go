// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package actionplan

import (
	"context"
	"testing"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

func wait(name string, scope stagesv1.ActionScope) stagesv1.Action {
	return stagesv1.Action{Name: name, Scope: scope, Wait: &stagesv1.WaitAction{Expr: "true"}}
}

// verdictFor returns the verdict for a named action, or a zero Verdict.
func verdictFor(vs []Verdict, name string) Verdict {
	for _, v := range vs {
		if v.Name == name {
			return v
		}
	}
	return Verdict{}
}

func TestActionVerdicts_Revision(t *testing.T) {
	stage := &stagesv1.Stage{Name: "app", Actions: &stagesv1.StageActions{
		Pre: []stagesv1.Action{wait("check", stagesv1.ScopeRevision)},
	}}

	// New revision (nothing recorded at it): runs.
	got := verdictFor(ActionVerdicts(context.Background(), nil, stage, VerdictInputs{Revision: "rev2"}), "check")
	if got.State != WillRun {
		t.Errorf("new revision: state = %q, want WILL RUN", got.State)
	}

	// Already ran at this revision: skipped.
	prior := stagesv1.StageStatus{LedgerRevision: "rev2", ExecutedActions: []string{"check"}}
	got = verdictFor(ActionVerdicts(context.Background(), nil, stage, VerdictInputs{Revision: "rev2", Prior: prior}), "check")
	if got.State != Skip {
		t.Errorf("recorded at revision: state = %q, want SKIP", got.State)
	}
}

func TestActionVerdicts_Version(t *testing.T) {
	stage := &stagesv1.Stage{Name: "app", Actions: &stagesv1.StageActions{
		Pre: []stagesv1.Action{wait("db-upgrade", stagesv1.ScopeVersion)},
	}}
	base := VerdictInputs{Revision: "rev2", Versioned: true, DesiredVersion: "2.0.0"}

	// Held at a fixed version (recorded in the version ledger): skipped despite the
	// new revision — the flagship config-churn-immunity case.
	in := base
	in.CurrentVersion = "2.0.0"
	in.Prior = stagesv1.StageStatus{LedgerVersion: "2.0.0", ExecutedVersionActions: []string{"db-upgrade"}}
	if got := verdictFor(ActionVerdicts(context.Background(), nil, stage, in), "db-upgrade"); got.State != Skip {
		t.Errorf("held at version: state = %q, want SKIP", got.State)
	}

	// A version change: new episode, runs.
	in = base
	in.CurrentVersion = "1.0.0"
	in.Prior = stagesv1.StageStatus{LedgerVersion: "1.0.0", ExecutedVersionActions: []string{"db-upgrade"}}
	if got := verdictFor(ActionVerdicts(context.Background(), nil, stage, in), "db-upgrade"); got.State != WillRun {
		t.Errorf("version change: state = %q, want WILL RUN", got.State)
	}

	// First adoption (status.version empty): baselined, not run.
	in = base
	in.CurrentVersion = ""
	if got := verdictFor(ActionVerdicts(context.Background(), nil, stage, in), "db-upgrade"); got.State != Skip {
		t.Errorf("adoption baseline: state = %q, want SKIP", got.State)
	}
}

func TestActionVerdicts_Lifetime(t *testing.T) {
	stage := &stagesv1.Stage{Name: "app", Actions: &stagesv1.StageActions{
		Post: []stagesv1.Action{wait("install-database", stagesv1.ScopeLifetime)},
	}}

	// Not yet recorded: runs.
	if got := verdictFor(ActionVerdicts(context.Background(), nil, stage, VerdictInputs{Revision: "rev2"}), "install-database"); got.State != WillRun {
		t.Errorf("unrecorded lifetime: state = %q, want WILL RUN", got.State)
	}

	// Completed (unanchored, so no witness read): skipped.
	ledger := &stagesv1.StageLedger{Status: stagesv1.StageLedgerStatus{
		CompletedActions: []stagesv1.LedgerCompletion{{Stage: "app", Action: "install-database", Origin: stagesv1.OriginExecuted}},
	}}
	if got := verdictFor(ActionVerdicts(context.Background(), nil, stage, VerdictInputs{Revision: "rev2", Lifetime: ledger}), "install-database"); got.State != Skip {
		t.Errorf("completed lifetime: state = %q, want SKIP", got.State)
	}
}

func TestActionVerdicts_ExcludesOnFailure(t *testing.T) {
	stage := &stagesv1.Stage{Name: "app", Actions: &stagesv1.StageActions{
		Pre:       []stagesv1.Action{wait("check", stagesv1.ScopeRevision)},
		OnFailure: []stagesv1.Action{wait("cleanup", "")},
	}}
	vs := ActionVerdicts(context.Background(), nil, stage, VerdictInputs{Revision: "rev2"})
	if verdictFor(vs, "cleanup").Name != "" {
		t.Error("onFailure action must not appear in the would-run preview")
	}
	if verdictFor(vs, "check").Name == "" {
		t.Error("pre action should appear")
	}
}
