// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

// Package actionplan holds the pure, side-effect-free decision predicates that
// answer "would this action run, and why" for a stage's pre/post actions. The
// reconciler runs these to gate execution; the read-only CLI previews (`diff`,
// `plan`) run the SAME functions to predict it. One code path means a preview
// cannot drift from what the controller actually does.
package actionplan

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

// AnchorState classifies a completionAnchor witness read at gate time.
type AnchorState int

const (
	// AnchorOK: the completion holds — no anchor, or the witness exists with the
	// recorded UID, or (fail open) the witness could not be read.
	AnchorOK AnchorState = iota
	// AnchorGone: the witness is confirmed absent or its UID changed — the
	// recorded effect no longer exists, so the action runs again.
	AnchorGone
	// AnchorUnreadable: the witness read failed for a reason other than NotFound.
	// Gated as retained, but surfaced so an operator can grant the missing read.
	AnchorUnreadable
)

// ClassifyAnchor is the pure verdict for one completion given the result of
// reading its witness. It encodes the fail-open rule: only a confirmed-absent or
// UID-changed witness re-runs the action; an unreadable one retains it.
func ClassifyAnchor(c *stagesv1.LedgerCompletion, obj *unstructured.Unstructured, readErr error) AnchorState {
	if c.Anchor == nil {
		return AnchorOK // unanchored: external effect, retained unconditionally
	}
	if readErr != nil {
		if apierrors.IsNotFound(readErr) {
			return AnchorGone // confirmed absent
		}
		return AnchorUnreadable // fail open: an outage must not re-run a bootstrap
	}
	if string(obj.GetUID()) != c.Anchor.UID {
		return AnchorGone // same name, fresh object — the recorded effect is gone
	}
	return AnchorOK
}

// anchorGVK parses a completionAnchor's apiVersion+kind.
func anchorGVK(apiVersion, kind string) (schema.GroupVersionKind, error) {
	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return schema.GroupVersionKind{}, fmt.Errorf("parse anchor apiVersion %q: %w", apiVersion, err)
	}
	return gv.WithKind(kind), nil
}

// ReadAnchorObject reads a witness object by (apiVersion, kind, name) in ns
// through the given reader — the stage's effective-SA client on the reconcile
// path, the caller's own client on a preview path.
func ReadAnchorObject(ctx context.Context, reader client.Client, ns, apiVersion, kind, name string) (*unstructured.Unstructured, error) {
	gvk, err := anchorGVK(apiVersion, kind)
	if err != nil {
		return nil, err
	}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	if err := reader.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, obj); err != nil {
		return nil, err
	}
	return obj, nil
}

// LifetimeGate is the outcome of evaluating a stage's recorded lifetime
// completions against their witnesses.
type LifetimeGate struct {
	// Done names the actions to treat as already complete (skip this reconcile).
	Done []string
	// Invalidated names the actions whose witness is gone; on the reconcile path
	// their completions are dropped so the action re-runs and re-records.
	Invalidated []string
	// Unreadable names the actions whose witness read failed; retained (fail open)
	// and surfaced.
	Unreadable []string
}

// EvaluateLifetimeGate reads every recorded completion's witness for one stage
// and classifies it. It performs cluster reads (through the given reader) but
// mutates nothing: the caller applies the consequences. Keeping the read
// separate from the mutation is what lets a preview reuse the "would this run"
// decision without touching the cluster.
func EvaluateLifetimeGate(ctx context.Context, reader client.Client, ledger *stagesv1.StageLedger, ns, stage string) LifetimeGate {
	var g LifetimeGate
	if ledger == nil {
		return g
	}
	for i := range ledger.Status.CompletedActions {
		c := &ledger.Status.CompletedActions[i]
		if c.Stage != stage {
			continue
		}
		if c.Anchor == nil {
			g.Done = append(g.Done, c.Action)
			continue
		}
		obj, err := ReadAnchorObject(ctx, reader, ns, c.Anchor.APIVersion, c.Anchor.Kind, c.Anchor.Name)
		switch ClassifyAnchor(c, obj, err) {
		case AnchorOK:
			g.Done = append(g.Done, c.Action)
		case AnchorUnreadable:
			g.Done = append(g.Done, c.Action) // fail open: retain
			g.Unreadable = append(g.Unreadable, c.Action)
		case AnchorGone:
			g.Invalidated = append(g.Invalidated, c.Action)
		}
	}
	return g
}

// ActionScopes maps each of a stage's pre+post action names to its effective
// scope. onFailure actions are excluded — they carry no scope and never enter a
// run ledger.
func ActionScopes(stage *stagesv1.Stage) map[string]stagesv1.ActionScope {
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
