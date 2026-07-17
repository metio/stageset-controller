// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import stagesv1 "github.com/metio/stageset-controller/api/v1"

// actionLedger is the per-stage execution ledger threaded through the stage loop
// and persisted on every exit (success, gate-interrupt, failStage). It carries
// both scopes so a partially-run ladder resumes at the failed action regardless
// of which ledger gates each step.
//
//   - revision + executed: the historical per-revision ledger. Reset by a new
//     revision and by a force-reconcile (both leave the version ledger alone).
//   - version + versioned: the per-version-episode ledger. version is "" on an
//     unversioned StageSet. Reset only when the resolved version changes — which
//     is detected here, at ledger assembly, mirroring the revision rekey. It is
//     never touched by the version-advance block or any other existing clearing
//     site, which is what keeps scope: Version purely additive.
type actionLedger struct {
	revision  string
	executed  []string
	version   string
	versioned []string
	// lifetimeDone holds this stage's scope: Lifetime action names already
	// recorded complete in the StageLedger — gate-only, never written back here
	// (Lifetime completions live in the StageLedger object, not stage status).
	lifetimeDone []string
}

// doneSet is the union of both ledgers' action names — the set the executor
// skips. A Version-scoped action in the version ledger and a Revision-scoped one
// in the revision ledger are both already done for this stage.
func (l actionLedger) doneSet() map[string]bool {
	done := make(map[string]bool, len(l.executed)+len(l.versioned)+len(l.lifetimeDone))
	for _, n := range l.executed {
		done[n] = true
	}
	for _, n := range l.versioned {
		done[n] = true
	}
	for _, n := range l.lifetimeDone {
		done[n] = true
	}
	return done
}

// recordFn returns the executor's per-action record callback, routing each
// completed action into the ledger its scope selects. scopeOf maps action name
// to scope over this stage's pre+post actions; an unlisted name (which cannot
// occur for a run action) defaults to the revision ledger.
func (l *actionLedger) recordFn(scopeOf map[string]stagesv1.ActionScope) func(string) error {
	return func(name string) error {
		if scopeOf[name] == stagesv1.ScopeVersion {
			l.versioned = append(l.versioned, name)
		} else {
			l.executed = append(l.executed, name)
		}
		return nil
	}
}

// stamp writes both ledgers onto a StageStatus. Every persist site routes
// through it so the revision and version fields never drift apart.
func (l actionLedger) stamp(s stagesv1.StageStatus) stagesv1.StageStatus {
	s.ExecutedActions = l.executed
	s.LedgerRevision = l.revision
	s.ExecutedVersionActions = l.versioned
	s.LedgerVersion = l.version
	return s
}

// stageActionScopes maps each of a stage's pre+post action names to its
// effective scope. onFailure actions are excluded — admission forbids scope
// there, and they never enter the run ledger.
func stageActionScopes(stage *stagesv1.Stage) map[string]stagesv1.ActionScope {
	if stage.Actions == nil {
		return nil
	}
	m := make(map[string]stagesv1.ActionScope, len(stage.Actions.Pre)+len(stage.Actions.Post))
	for i := range stage.Actions.Pre {
		m[stage.Actions.Pre[i].Name] = stage.Actions.Pre[i].EffectiveScope()
	}
	for i := range stage.Actions.Post {
		m[stage.Actions.Post[i].Name] = stage.Actions.Post[i].EffectiveScope()
	}
	return m
}

// versionScopedActionNames lists a stage's pre+post action names scoped to
// Version — the set the first-adoption baseline seeds as already-done.
func versionScopedActionNames(stage *stagesv1.Stage) []string {
	if stage.Actions == nil {
		return nil
	}
	var out []string
	for i := range stage.Actions.Pre {
		if stage.Actions.Pre[i].EffectiveScope() == stagesv1.ScopeVersion {
			out = append(out, stage.Actions.Pre[i].Name)
		}
	}
	for i := range stage.Actions.Post {
		if stage.Actions.Post[i].EffectiveScope() == stagesv1.ScopeVersion {
			out = append(out, stage.Actions.Post[i].Name)
		}
	}
	return out
}
