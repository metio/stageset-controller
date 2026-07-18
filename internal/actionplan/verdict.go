// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package actionplan

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

// RunState is an action's predicted disposition on the next reconcile.
type RunState string

const (
	// WillRun: the action executes on the next reconcile.
	WillRun RunState = "WILL RUN"
	// Skip: a ledger already satisfies the action for its scope.
	Skip RunState = "SKIP"
	// ReRun: a scope: Lifetime action was completed but its completionAnchor
	// witness is gone, so it runs again against the empty state.
	ReRun RunState = "RE-RUN"
)

// Verdict is one pre/post action's predicted disposition, with the reason. It is
// a prediction of what the controller will ATTEMPT — never of the outcome.
type Verdict struct {
	Stage  string
	Phase  string // "pre" or "post"
	Name   string
	Scope  stagesv1.ActionScope
	State  RunState
	Reason string
}

// VerdictInputs carries the resolved facts a stage's action verdicts depend on —
// gathered once by the caller (rendered revision, resolved version, ledgers) and
// held constant while every action is classified.
type VerdictInputs struct {
	Namespace string
	// Revision is the rendered next revision, empty for a local render (which
	// cannot consult the per-revision ledger).
	Revision string
	// Versioned is true when spec.version is set; DesiredVersion is the resolved
	// value, CurrentVersion is status.version.
	Versioned      bool
	DesiredVersion string
	CurrentVersion string
	// Prior is the stage's last recorded status (its revision and version ledgers).
	Prior stagesv1.StageStatus
	// Lifetime is the StageLedger (nil when none exists).
	Lifetime *stagesv1.StageLedger
}

// ActionVerdicts predicts, for every pre/post action of a stage, whether the next
// reconcile will run, skip, or re-run it, and why. onFailure actions are
// excluded: they run only on a failure, so they are not part of a would-run
// preview. It mirrors the reconciler's gate — reusing the same scope map,
// lifetime gate, and revision/version ledger rules — so a preview agrees with
// what the controller does. It reads witness objects through reader but mutates
// nothing.
func ActionVerdicts(ctx context.Context, reader client.Client, stage *stagesv1.Stage, in VerdictInputs) []Verdict {
	if stage.Actions == nil {
		return nil
	}
	scopes := ActionScopes(stage)

	revDone := map[string]bool{}
	if in.Revision != "" && in.Prior.LedgerRevision == in.Revision {
		for _, n := range in.Prior.ExecutedActions {
			revDone[n] = true
		}
	}
	verDone := map[string]bool{}
	if in.Versioned && in.Prior.LedgerVersion == in.DesiredVersion {
		for _, n := range in.Prior.ExecutedVersionActions {
			verDone[n] = true
		}
	}
	// First adoption of a versioned StageSet baselines its version-scoped actions:
	// they are recorded without running, so a preview must show them skipped.
	baseline := in.Versioned && in.CurrentVersion == ""

	gate := EvaluateLifetimeGate(ctx, reader, in.Lifetime, in.Namespace, stage.Name)
	lifeDone := map[string]bool{}
	for _, n := range gate.Done {
		lifeDone[n] = true
	}
	lifeReRun := map[string]bool{}
	for _, n := range gate.Invalidated {
		lifeReRun[n] = true
	}

	var out []Verdict
	classify := func(phase string, list []stagesv1.Action) {
		for i := range list {
			name := list[i].Name
			sc := scopes[name]
			v := Verdict{Stage: stage.Name, Phase: phase, Name: name, Scope: sc}
			switch sc {
			case stagesv1.ScopeLifetime:
				switch {
				case lifeReRun[name]:
					v.State, v.Reason = ReRun, "completionAnchor witness is gone"
				case lifeDone[name]:
					v.State, v.Reason = Skip, "completed once, ever"
				default:
					v.State, v.Reason = WillRun, "once ever, not yet recorded"
				}
			case stagesv1.ScopeVersion:
				switch {
				case verDone[name]:
					v.State, v.Reason = Skip, fmt.Sprintf("held at version %s", in.DesiredVersion)
				case baseline:
					v.State, v.Reason = Skip, fmt.Sprintf("baselined on adoption at %s", in.DesiredVersion)
				case in.CurrentVersion != "" && in.CurrentVersion != in.DesiredVersion:
					v.State, v.Reason = WillRun, fmt.Sprintf("new version episode %s → %s", in.CurrentVersion, in.DesiredVersion)
				default:
					v.State, v.Reason = WillRun, fmt.Sprintf("version %s", in.DesiredVersion)
				}
			default: // Revision (and "")
				if revDone[name] {
					v.State, v.Reason = Skip, "already ran at this revision"
				} else if in.Revision == "" {
					v.State, v.Reason = WillRun, "local render; ledger not consulted"
				} else {
					v.State, v.Reason = WillRun, "new revision"
				}
			}
			out = append(out, v)
		}
	}
	classify("pre", stage.Actions.Pre)
	classify("post", stage.Actions.Post)
	return out
}
