// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package inventory

import (
	"cmp"
	"slices"
)

// StageRecord is a stage's recorded inventory: its name, its position in
// spec.stages at the time it was recorded (used for reverse-order teardown),
// and the objects it owns.
type StageRecord struct {
	Name     string
	Position int
	Entries  []ObjectRef
}

// Plan describes every deletion the controller may perform in one
// reconciliation run. It is purely descriptive: per-stage prune toggles,
// per-object opt-out annotations, and DeletionPolicy are applied by the
// caller on top of this plan.
type Plan struct {
	// PrunePerStage maps a surviving stage name to the objects that fell
	// out of that stage and are not claimed by any desired stage. Stages
	// with nothing to prune are absent.
	PrunePerStage map[string][]ObjectRef

	// RemovedStages lists previously recorded stages that no longer exist
	// in the desired spec, ordered for teardown: descending Position
	// (later stages first), name as tie-break. Entries claimed by any
	// desired stage have been removed (ownership transfer).
	RemovedStages []StageRecord
}

// ComputePlan diffs the previously recorded inventories against the desired
// state of the current run and returns the deletions that would converge the
// cluster. Objects that merely moved between stages are transferred, never
// deleted: any object claimed by any desired stage is exempt from pruning
// and teardown.
func ComputePlan(previous, desired []StageRecord) Plan {
	claimed := make(map[string]struct{})
	desiredByName := make(map[string]map[string]struct{}, len(desired))
	for _, stage := range desired {
		ids := make(map[string]struct{}, len(stage.Entries))
		for _, ref := range stage.Entries {
			id := ref.ID()
			ids[id] = struct{}{}
			claimed[id] = struct{}{}
		}
		desiredByName[stage.Name] = ids
	}

	plan := Plan{PrunePerStage: make(map[string][]ObjectRef)}
	for _, prev := range previous {
		desiredIDs, survives := desiredByName[prev.Name]
		if survives {
			prune := filterRefs(prev.Entries, func(id string) bool {
				_, still := desiredIDs[id]
				_, moved := claimed[id]
				return !still && !moved
			})
			if len(prune) > 0 {
				plan.PrunePerStage[prev.Name] = prune
			}
			continue
		}
		teardown := filterRefs(prev.Entries, func(id string) bool {
			_, moved := claimed[id]
			return !moved
		})
		plan.RemovedStages = append(plan.RemovedStages, StageRecord{
			Name:     prev.Name,
			Position: prev.Position,
			Entries:  teardown,
		})
	}

	slices.SortFunc(plan.RemovedStages, func(a, b StageRecord) int {
		if c := cmp.Compare(b.Position, a.Position); c != 0 {
			return c
		}
		return cmp.Compare(a.Name, b.Name)
	})
	return plan
}

// DuplicateClaims returns every object ID that is claimed by more than one
// of the given stages, mapped to the sorted names of the claiming stages.
// The controller uses this to reject ambiguous specs before applying
// anything: an object owned by two stages would make prune and teardown
// semantics undefined.
func DuplicateClaims(stages []StageRecord) map[string][]string {
	owners := make(map[string][]string)
	for _, stage := range stages {
		seen := make(map[string]struct{}, len(stage.Entries))
		for _, ref := range stage.Entries {
			id := ref.ID()
			if _, dup := seen[id]; dup {
				continue // duplicates within one stage are harmless after build dedup
			}
			seen[id] = struct{}{}
			owners[id] = append(owners[id], stage.Name)
		}
	}
	duplicates := make(map[string][]string)
	for id, names := range owners {
		if len(names) > 1 {
			slices.Sort(names)
			duplicates[id] = names
		}
	}
	return duplicates
}

func filterRefs(refs []ObjectRef, keep func(id string) bool) []ObjectRef {
	var out []ObjectRef
	for _, ref := range refs {
		if keep(ref.ID()) {
			out = append(out, ref)
		}
	}
	slices.SortFunc(out, func(a, b ObjectRef) int { return cmp.Compare(a.ID(), b.ID()) })
	return out
}
