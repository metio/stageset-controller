// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

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

// anchorState classifies a completionAnchor witness read at gate time.
type anchorState int

const (
	// anchorOK: the completion holds — no anchor, or the witness exists with the
	// recorded UID, or (fail open) the witness could not be read.
	anchorOK anchorState = iota
	// anchorGone: the witness is confirmed absent or its UID changed — the
	// recorded effect no longer exists, so the action runs again.
	anchorGone
	// anchorUnreadable: the witness read failed for a reason other than NotFound.
	// Gated as retained, but surfaced so an operator can grant the missing read.
	anchorUnreadable
)

// classifyAnchor is the pure verdict for one completion given the result of
// reading its witness. It is side-effect free — a future `plan` preview reuses
// it — and encodes the fail-open rule: only a confirmed-absent or UID-changed
// witness re-runs the action; an unreadable one retains it.
func classifyAnchor(c *stagesv1.LedgerCompletion, obj *unstructured.Unstructured, readErr error) anchorState {
	if c.Anchor == nil {
		return anchorOK // unanchored: external effect, retained unconditionally
	}
	if readErr != nil {
		if apierrors.IsNotFound(readErr) {
			return anchorGone // confirmed absent
		}
		return anchorUnreadable // fail open: an outage must not re-run a bootstrap
	}
	if string(obj.GetUID()) != c.Anchor.UID {
		return anchorGone // same name, fresh object — the recorded effect is gone
	}
	return anchorOK
}

// anchorGVK parses a completionAnchor's apiVersion+kind.
func anchorGVK(apiVersion, kind string) (schema.GroupVersionKind, error) {
	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return schema.GroupVersionKind{}, fmt.Errorf("parse anchor apiVersion %q: %w", apiVersion, err)
	}
	return gv.WithKind(kind), nil
}

// readAnchorObject reads a witness object by (apiVersion, kind, name) in ns
// through the stage's effective-SA client.
func readAnchorObject(ctx context.Context, stageClient client.Client, ns, apiVersion, kind, name string) (*unstructured.Unstructured, error) {
	gvk, err := anchorGVK(apiVersion, kind)
	if err != nil {
		return nil, err
	}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	if err := stageClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, obj); err != nil {
		return nil, err
	}
	return obj, nil
}

// lifetimeGate is the outcome of evaluating a stage's recorded lifetime
// completions against their witnesses.
type lifetimeGate struct {
	// done names the actions to treat as already complete (skip this reconcile).
	done []string
	// invalidated names the actions whose witness is gone; their completions must
	// be dropped so the action re-runs and re-records.
	invalidated []string
	// unreadable names the actions whose witness read failed; retained (fail
	// open) and surfaced.
	unreadable []string
}

// evaluateLifetimeGate reads every recorded completion's witness for one stage
// and classifies it. It performs cluster reads (through the stage's effective-SA
// client) but mutates nothing: the caller applies the consequences (drop the
// invalidated completions, emit events, bump the metric). Keeping the read
// separate from the mutation is what lets the "would this run" decision be
// reused by a preview that must not touch the cluster.
func (r *StageSetReconciler) evaluateLifetimeGate(ctx context.Context, stageClient client.Client, ledger *stagesv1.StageLedger, ns, stage string) lifetimeGate {
	var g lifetimeGate
	if ledger == nil {
		return g
	}
	for i := range ledger.Status.CompletedActions {
		c := &ledger.Status.CompletedActions[i]
		if c.Stage != stage {
			continue
		}
		if c.Anchor == nil {
			g.done = append(g.done, c.Action)
			continue
		}
		obj, err := readAnchorObject(ctx, stageClient, ns, c.Anchor.APIVersion, c.Anchor.Kind, c.Anchor.Name)
		switch classifyAnchor(c, obj, err) {
		case anchorOK:
			g.done = append(g.done, c.Action)
		case anchorUnreadable:
			g.done = append(g.done, c.Action) // fail open: retain
			g.unreadable = append(g.unreadable, c.Action)
		case anchorGone:
			g.invalidated = append(g.invalidated, c.Action)
		}
	}
	return g
}

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
