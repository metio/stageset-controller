// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package inventory

import (
	"reflect"
	"testing"
)

func deployment(ns, name string) ObjectRef {
	return ObjectRef{Group: "apps", Kind: "Deployment", Namespace: ns, Name: name, Version: "v1"}
}

func configMap(ns, name string) ObjectRef {
	return ObjectRef{Kind: "ConfigMap", Namespace: ns, Name: name, Version: "v1"}
}

func TestComputePlanPrunesRemovedObjects(t *testing.T) {
	t.Parallel()
	previous := []StageRecord{{
		Name:     "apps",
		Position: 2,
		Entries:  []ObjectRef{deployment("a", "keep"), deployment("a", "drop"), configMap("a", "drop-too")},
	}}
	desired := []StageRecord{{
		Name:     "apps",
		Position: 2,
		Entries:  []ObjectRef{deployment("a", "keep"), deployment("a", "new")},
	}}

	plan := ComputePlan(previous, desired)

	want := []ObjectRef{configMap("a", "drop-too"), deployment("a", "drop")}
	if !reflect.DeepEqual(plan.PrunePerStage["apps"], want) {
		t.Fatalf("prune list = %+v, want %+v", plan.PrunePerStage["apps"], want)
	}
	if len(plan.RemovedStages) != 0 {
		t.Fatalf("unexpected removed stages: %+v", plan.RemovedStages)
	}
}

func TestComputePlanOmitsStagesWithNothingToPrune(t *testing.T) {
	t.Parallel()
	records := []StageRecord{{Name: "crds", Position: 0, Entries: []ObjectRef{configMap("a", "x")}}}
	plan := ComputePlan(records, records)
	if len(plan.PrunePerStage) != 0 {
		t.Fatalf("expected empty prune map, got %+v", plan.PrunePerStage)
	}
}

func TestComputePlanTransfersOwnershipBetweenStages(t *testing.T) {
	t.Parallel()
	moved := deployment("a", "moved")
	previous := []StageRecord{
		{Name: "operators", Position: 1, Entries: []ObjectRef{moved, deployment("a", "stays")}},
		{Name: "apps", Position: 2, Entries: []ObjectRef{deployment("a", "app")}},
	}
	desired := []StageRecord{
		{Name: "operators", Position: 1, Entries: []ObjectRef{deployment("a", "stays")}},
		{Name: "apps", Position: 2, Entries: []ObjectRef{deployment("a", "app"), moved}},
	}

	plan := ComputePlan(previous, desired)

	if refs, ok := plan.PrunePerStage["operators"]; ok {
		t.Fatalf("moved object must not be pruned, got %+v", refs)
	}
}

func TestComputePlanRemovedStagesInReverseOrder(t *testing.T) {
	t.Parallel()
	previous := []StageRecord{
		{Name: "crds", Position: 0, Entries: []ObjectRef{configMap("a", "crd-cm")}},
		{Name: "operators", Position: 1, Entries: []ObjectRef{deployment("a", "op")}},
		{Name: "apps", Position: 2, Entries: []ObjectRef{deployment("a", "app")}},
	}
	plan := ComputePlan(previous, nil)

	gotOrder := make([]string, 0, len(plan.RemovedStages))
	for _, stage := range plan.RemovedStages {
		gotOrder = append(gotOrder, stage.Name)
	}
	wantOrder := []string{"apps", "operators", "crds"}
	if !reflect.DeepEqual(gotOrder, wantOrder) {
		t.Fatalf("teardown order = %v, want %v", gotOrder, wantOrder)
	}
}

func TestComputePlanRemovedStageOrderTieBreaksByName(t *testing.T) {
	t.Parallel()
	previous := []StageRecord{
		{Name: "zeta", Position: 1},
		{Name: "alpha", Position: 1},
	}
	plan := ComputePlan(previous, nil)
	if plan.RemovedStages[0].Name != "alpha" || plan.RemovedStages[1].Name != "zeta" {
		t.Fatalf("tie-break order wrong: %+v", plan.RemovedStages)
	}
}

func TestComputePlanRemovedStageExcludesObjectsClaimedElsewhere(t *testing.T) {
	t.Parallel()
	rescued := deployment("a", "rescued")
	previous := []StageRecord{
		{Name: "legacy", Position: 0, Entries: []ObjectRef{rescued, deployment("a", "gone")}},
		{Name: "apps", Position: 1, Entries: []ObjectRef{deployment("a", "app")}},
	}
	desired := []StageRecord{
		{Name: "apps", Position: 1, Entries: []ObjectRef{deployment("a", "app"), rescued}},
	}

	plan := ComputePlan(previous, desired)

	if len(plan.RemovedStages) != 1 || plan.RemovedStages[0].Name != "legacy" {
		t.Fatalf("expected exactly the legacy stage to be removed, got %+v", plan.RemovedStages)
	}
	want := []ObjectRef{deployment("a", "gone")}
	if !reflect.DeepEqual(plan.RemovedStages[0].Entries, want) {
		t.Fatalf("teardown entries = %+v, want %+v", plan.RemovedStages[0].Entries, want)
	}
}

func TestComputePlanEmptyPrevious(t *testing.T) {
	t.Parallel()
	plan := ComputePlan(nil, []StageRecord{{Name: "apps", Entries: []ObjectRef{deployment("a", "x")}}})
	if len(plan.PrunePerStage) != 0 || len(plan.RemovedStages) != 0 {
		t.Fatalf("first run must plan no deletions, got %+v", plan)
	}
}

func TestDuplicateClaims(t *testing.T) {
	t.Parallel()
	shared := deployment("a", "shared")
	stages := []StageRecord{
		{Name: "operators", Entries: []ObjectRef{shared, deployment("a", "unique-op")}},
		{Name: "apps", Entries: []ObjectRef{shared, deployment("a", "unique-app")}},
		{Name: "crds", Entries: []ObjectRef{configMap("a", "solo"), configMap("a", "solo")}}, // intra-stage dup is fine
	}

	duplicates := DuplicateClaims(stages)

	if len(duplicates) != 1 {
		t.Fatalf("expected exactly one duplicate claim, got %+v", duplicates)
	}
	owners, ok := duplicates[shared.ID()]
	if !ok {
		t.Fatalf("expected duplicate for %q, got %+v", shared.ID(), duplicates)
	}
	if !reflect.DeepEqual(owners, []string{"apps", "operators"}) {
		t.Fatalf("owners = %v, want sorted [apps operators]", owners)
	}
}
