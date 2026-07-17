// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"reflect"
	"testing"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

func TestActionLedger_DoneSetUnionsBothScopes(t *testing.T) {
	led := actionLedger{
		executed:  []string{"rev-a", "rev-b"},
		versioned: []string{"ver-a"},
	}
	done := led.doneSet()
	for _, n := range []string{"rev-a", "rev-b", "ver-a"} {
		if !done[n] {
			t.Errorf("doneSet missing %q; a completed action of either scope must be skipped", n)
		}
	}
	if done["never"] {
		t.Error("doneSet contains an action that was never recorded")
	}
}

func TestActionLedger_RecordFnRoutesByScope(t *testing.T) {
	var led actionLedger
	scopeOf := map[string]stagesv1.ActionScope{
		"upgrade": stagesv1.ScopeVersion,
		"check":   stagesv1.ScopeRevision,
		"blank":   "", // unset defaults to the revision ledger
	}
	rec := led.recordFn(scopeOf)
	for _, n := range []string{"check", "upgrade", "blank"} {
		if err := rec(n); err != nil {
			t.Fatalf("record %q: %v", n, err)
		}
	}
	if want := []string{"check", "blank"}; !reflect.DeepEqual(led.executed, want) {
		t.Errorf("revision ledger = %v, want %v", led.executed, want)
	}
	if want := []string{"upgrade"}; !reflect.DeepEqual(led.versioned, want) {
		t.Errorf("version ledger = %v, want %v", led.versioned, want)
	}
}

// An action name absent from the scope map (which cannot happen for a run
// action, but guards against a routing panic) lands in the revision ledger.
func TestActionLedger_RecordFnUnlistedNameIsRevision(t *testing.T) {
	var led actionLedger
	rec := led.recordFn(nil)
	_ = rec("orphan")
	if len(led.versioned) != 0 || len(led.executed) != 1 {
		t.Fatalf("unlisted action routed to %v/%v, want the revision ledger", led.executed, led.versioned)
	}
}

func TestActionLedger_StampWritesBothPairs(t *testing.T) {
	led := actionLedger{
		revision:  "sha256:rev",
		executed:  []string{"check"},
		version:   "1.2.3",
		versioned: []string{"upgrade"},
	}
	s := led.stamp(stagesv1.StageStatus{Name: "app"})
	if s.LedgerRevision != "sha256:rev" || !reflect.DeepEqual(s.ExecutedActions, []string{"check"}) {
		t.Errorf("revision ledger not stamped: %+v", s)
	}
	if s.LedgerVersion != "1.2.3" || !reflect.DeepEqual(s.ExecutedVersionActions, []string{"upgrade"}) {
		t.Errorf("version ledger not stamped: %+v", s)
	}
	if s.Name != "app" {
		t.Errorf("stamp clobbered an unrelated field: Name = %q", s.Name)
	}
}

func TestVersionScopedActionNames(t *testing.T) {
	stage := &stagesv1.Stage{Actions: &stagesv1.StageActions{
		Pre:  []stagesv1.Action{{Name: "maint-on", Scope: stagesv1.ScopeVersion}, {Name: "check"}},
		Post: []stagesv1.Action{{Name: "maint-off", Scope: stagesv1.ScopeVersion}},
	}}
	got := versionScopedActionNames(stage)
	want := []string{"maint-on", "maint-off"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("version-scoped names = %v, want %v", got, want)
	}
	if names := versionScopedActionNames(&stagesv1.Stage{}); names != nil {
		t.Errorf("a stage with no actions returned %v, want nil", names)
	}
}
