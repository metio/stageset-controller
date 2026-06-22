// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestStageReadyGauge(t *testing.T) {
	StageReady.Reset()

	SetStageReady("ns", "app", "infra", true)
	SetStageReady("ns", "app", "web", false)

	if v := testutil.ToFloat64(StageReady.WithLabelValues("ns", "app", "infra")); v != 1 {
		t.Fatalf("infra gauge = %v, want 1", v)
	}
	if v := testutil.ToFloat64(StageReady.WithLabelValues("ns", "app", "web")); v != 0 {
		t.Fatalf("web gauge = %v, want 0", v)
	}

	// Re-setting flips the value in place.
	SetStageReady("ns", "app", "web", true)
	if v := testutil.ToFloat64(StageReady.WithLabelValues("ns", "app", "web")); v != 1 {
		t.Fatalf("web gauge after flip = %v, want 1", v)
	}
}

func TestDeleteStageReady(t *testing.T) {
	StageReady.Reset()

	SetStageReady("ns", "app", "infra", true)
	SetStageReady("ns", "app", "web", true)
	SetStageReady("ns", "other", "x", true) // a different StageSet must survive

	DeleteStageReady("ns", "app")

	if n := testutil.CollectAndCount(StageReady); n != 1 {
		t.Fatalf("series after delete = %d, want 1 (only ns/other/x)", n)
	}
	if v := testutil.ToFloat64(StageReady.WithLabelValues("ns", "other", "x")); v != 1 {
		t.Fatalf("unrelated StageSet gauge = %v, want 1", v)
	}
}

func TestDeleteStageReadyForStage(t *testing.T) {
	StageReady.Reset()

	SetStageReady("ns", "app", "infra", true)
	SetStageReady("ns", "app", "web", true)

	// A stage dropped from the spec must lose its series; its siblings survive.
	DeleteStageReadyForStage("ns", "app", "web")

	if n := testutil.CollectAndCount(StageReady); n != 1 {
		t.Fatalf("series after per-stage delete = %d, want 1 (only infra)", n)
	}
	if v := testutil.ToFloat64(StageReady.WithLabelValues("ns", "app", "infra")); v != 1 {
		t.Fatalf("sibling stage gauge = %v, want 1", v)
	}
}

// TestDeleteStageSetMetrics proves a deleted StageSet leaves no orphaned
// operational series, while the teardown force-drop counter (the deletion-time
// alert signal) survives, and a sibling StageSet's series are untouched.
func TestDeleteStageSetMetrics(t *testing.T) {
	ReconcileTotal.Reset()
	StageAppliedTotal.Reset()
	DriftCorrectedTotal.Reset()
	UpdateDeferredTotal.Reset()
	InventorySkippedEntriesTotal.Reset()
	TeardownForceDropTotal.Reset()

	const ns, name, other = "ns", "doomed", "survivor"
	ReconcileTotal.WithLabelValues(ns, name, "Succeeded").Inc()
	ReconcileTotal.WithLabelValues(ns, name, "Failed").Inc() // second series, same ns/name
	StageAppliedTotal.WithLabelValues(ns, name, "infra").Inc()
	DriftCorrectedTotal.WithLabelValues(ns, name, "web").Inc()
	UpdateDeferredTotal.WithLabelValues(ns, name).Inc()
	InventorySkippedEntriesTotal.WithLabelValues(ns, name, "infra").Inc()
	TeardownForceDropTotal.WithLabelValues(ns, name).Inc()
	ReconcileTotal.WithLabelValues(ns, other, "Succeeded").Inc() // survivor

	before := testutil.CollectAndCount(ReconcileTotal) + testutil.CollectAndCount(StageAppliedTotal) +
		testutil.CollectAndCount(DriftCorrectedTotal) + testutil.CollectAndCount(UpdateDeferredTotal) +
		testutil.CollectAndCount(InventorySkippedEntriesTotal)

	DeleteStageSetMetrics(ns, name)

	after := testutil.CollectAndCount(ReconcileTotal) + testutil.CollectAndCount(StageAppliedTotal) +
		testutil.CollectAndCount(DriftCorrectedTotal) + testutil.CollectAndCount(UpdateDeferredTotal) +
		testutil.CollectAndCount(InventorySkippedEntriesTotal)

	// Doomed StageSet contributed 6 operational series (two reconcile + applied +
	// drift + deferred + inventory); all must vanish.
	if before-after != 6 {
		t.Errorf("DeleteStageSetMetrics removed %d operational series, want 6", before-after)
	}
	// The survivor's reconcile series must remain.
	if v := testutil.ToFloat64(ReconcileTotal.WithLabelValues(ns, other, "Succeeded")); v != 1 {
		t.Errorf("survivor series = %v, want 1 (over-deleted)", v)
	}
	// The force-drop counter is a deletion-time alert signal; it must survive.
	if v := testutil.ToFloat64(TeardownForceDropTotal.WithLabelValues(ns, name)); v != 1 {
		t.Errorf("teardown force-drop series = %v, want 1 (must survive)", v)
	}
}
