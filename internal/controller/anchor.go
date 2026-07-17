// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import stagesv1 "github.com/metio/stageset-controller/api/v1"

// The pure anchor/scope decision predicates live in internal/actionplan, shared
// with the read-only CLI previews. This file keeps only the reconciler's
// write-path helpers, which mutate ledger state and so are not part of a preview.

const (
	// eventReasonLedgerInvalidated marks a scope: Lifetime completion dropped
	// because its completionAnchor witness is gone (absent or a fresh UID) — the
	// action will run again against the empty state.
	eventReasonLedgerInvalidated = "LedgerInvalidated"
	// eventReasonLedgerAnchorUnreadable marks a completionAnchor that could not be
	// read at gate time. The completion is retained (fail open); the event points
	// at the missing read grant.
	eventReasonLedgerAnchorUnreadable = "LedgerAnchorUnreadable"
	// eventReasonLedgerAdopted marks a StageSet's first reconcile adopting a
	// StageLedger that already carries completions — a delete+recreate, or a fresh
	// StageSet over a retained ledger. The loud signal for the retain-always
	// surprise: an adopted completion may suppress an action that would otherwise
	// run.
	eventReasonLedgerAdopted = "LedgerAdopted"
)

// dropCompletions removes the named completions for a stage from the ledger's
// status, so an invalidated anchored action records a fresh completion when it
// re-runs. Returns whether anything changed.
func dropCompletions(ledger *stagesv1.StageLedger, stage string, actions []string) bool {
	if len(actions) == 0 {
		return false
	}
	drop := make(map[string]bool, len(actions))
	for _, a := range actions {
		drop[a] = true
	}
	kept := make([]stagesv1.LedgerCompletion, 0, len(ledger.Status.CompletedActions))
	changed := false
	for _, c := range ledger.Status.CompletedActions {
		if c.Stage == stage && drop[c.Action] {
			changed = true
			continue
		}
		kept = append(kept, c)
	}
	ledger.Status.CompletedActions = kept
	return changed
}

// stageAnchors maps a stage's pre+post action names to their completionAnchor
// (nil entries omitted), so the record path can witness an anchored action.
func stageAnchors(stage *stagesv1.Stage) map[string]*stagesv1.CompletionAnchor {
	if stage.Actions == nil {
		return nil
	}
	m := map[string]*stagesv1.CompletionAnchor{}
	for _, list := range [][]stagesv1.Action{stage.Actions.Pre, stage.Actions.Post} {
		for i := range list {
			if list[i].CompletionAnchor != nil {
				m[list[i].Name] = list[i].CompletionAnchor
			}
		}
	}
	return m
}
