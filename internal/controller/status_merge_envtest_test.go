// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

func stageStatusFor(ss *stagesv1.StageSet, name string) *stagesv1.StageStatus {
	for i := range ss.Status.Stages {
		if ss.Status.Stages[i].Name == name {
			return &ss.Status.Stages[i]
		}
	}
	return nil
}

// A promotion hold at an earlier stage must not erase the persisted records of
// the stages after it: their applied revision, ledger, and (crucially)
// lastHandledPromotion still describe live state the pass never touched.
func TestReconcile_PromotionHold_PreservesLaterStageStatus(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	servedArtifact(t, c, ns, "ea-1", "", map[string]string{"cm.yaml": cmValManifest(ns, "hold-1", "v1")})
	servedArtifact(t, c, ns, "ea-2", "", map[string]string{"cm.yaml": cmValManifest(ns, "hold-2", "v1")})

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "hold-preserve"},
		Spec: stagesv1.StageSetSpec{
			Interval: metav1.Duration{Duration: time.Minute},
			Stages: []stagesv1.Stage{
				{Name: "stage-1", SourceRef: stagesv1.SourceReference{Name: "ea-1"}},
				{Name: "stage-2", SourceRef: stagesv1.SourceReference{Name: "ea-2"}},
			},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	// Pass 1: both stages apply; status records both.
	reconcileAt(t, c, ss, t0)
	got := getStageSet(t, c, ns, "hold-preserve")
	if len(got.Status.Stages) != 2 {
		t.Fatalf("pass 1 should record both stages, got %+v", got.Status.Stages)
	}
	stage2Rev := stageStatusFor(got, "stage-2").AppliedRevision

	// Give stage-1 a soak and a new revision: pass 2 holds at stage-1 soaking.
	got.Spec.Stages[0].Promotion = &stagesv1.StagePromotion{Soak: dur(time.Hour)}
	if err := c.Update(context.Background(), got); err != nil {
		t.Fatalf("add soak: %v", err)
	}
	repointArtifact(t, c, ns, "ea-1", map[string]string{"cm.yaml": cmValManifest(ns, "hold-1", "v2")})
	reconcileAt(t, c, getStageSet(t, c, ns, "hold-preserve"), t0.Add(time.Minute))

	held := getStageSet(t, c, ns, "hold-preserve")
	s2 := stageStatusFor(held, "stage-2")
	if s2 == nil {
		t.Fatalf("stage-2's persisted status was erased by the hold at stage-1: %+v", held.Status.Stages)
	}
	if s2.AppliedRevision != stage2Rev {
		t.Errorf("stage-2 appliedRevision = %q, want the preserved %q", s2.AppliedRevision, stage2Rev)
	}
}

// A budget hold at an earlier stage goes through the errHoldForBudget handler —
// it must preserve later stages' records the same way.
func TestReconcile_BudgetHold_PreservesLaterStageStatus(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	servedArtifact(t, c, ns, "ea-1", "", map[string]string{"cm.yaml": cmValManifest(ns, "bh-1", "v1")})
	servedArtifact(t, c, ns, "ea-2", "", map[string]string{"cm.yaml": cmValManifest(ns, "bh-2", "v1")})

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "budget-preserve"},
		Spec: stagesv1.StageSetSpec{
			Interval: metav1.Duration{Duration: time.Minute},
			Stages: []stagesv1.Stage{
				{Name: "stage-1", SourceRef: stagesv1.SourceReference{Name: "ea-1"}},
				{Name: "stage-2", SourceRef: stagesv1.SourceReference{Name: "ea-2"}},
			},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	start := tm("2026-01-01T00:00:00Z").Time

	// Pass 1 (healthy budget): both stages apply.
	reconcileWithQuerier(t, c, ss, start, &fakeQuerier{value: 0.9})
	got := getStageSet(t, c, ns, "budget-preserve")
	if len(got.Status.Stages) != 2 {
		t.Fatalf("pass 1 should record both stages, got %+v", got.Status.Stages)
	}

	// Give stage-1 an errorBudget and a new revision; the budget is exhausted →
	// pass 2 holds at stage-1 (errHoldForBudget).
	got.Spec.Stages[0].ErrorBudget = &stagesv1.ErrorBudget{Source: promSource(), FreezeThreshold: "0.1"}
	if err := c.Update(context.Background(), got); err != nil {
		t.Fatalf("add errorBudget: %v", err)
	}
	repointArtifact(t, c, ns, "ea-1", map[string]string{"cm.yaml": cmValManifest(ns, "bh-1", "v2")})
	reconcileWithQuerier(t, c, getStageSet(t, c, ns, "budget-preserve"), start.Add(time.Minute), &fakeQuerier{value: 0.0})

	held := getStageSet(t, c, ns, "budget-preserve")
	if s1 := stageStatusFor(held, "stage-1"); s1 == nil || s1.BudgetFreeze == nil {
		t.Fatalf("stage-1 should be budget-held, got %+v", held.Status.Stages)
	}
	if s2 := stageStatusFor(held, "stage-2"); s2 == nil {
		t.Fatalf("stage-2's persisted status was erased by the budget hold at stage-1: %+v", held.Status.Stages)
	}
}

// A stage failure must not erase the records of the stages after the failed
// one — failStage writes only the stages processed up to the failure plus the
// failed entry, and later stages' ledgers/promotion records must survive.
func TestReconcile_StageFailure_PreservesLaterStageStatus(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	servedArtifact(t, c, ns, "ea-1", "", map[string]string{"cm.yaml": cmValManifest(ns, "sf-1", "v1")})
	servedArtifact(t, c, ns, "ea-2", "", map[string]string{"cm.yaml": cmValManifest(ns, "sf-2", "v1")})

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "fail-preserve"},
		Spec: stagesv1.StageSetSpec{
			Interval: metav1.Duration{Duration: time.Minute},
			Stages: []stagesv1.Stage{
				{Name: "stage-1", SourceRef: stagesv1.SourceReference{Name: "ea-1"}},
				{Name: "stage-2", SourceRef: stagesv1.SourceReference{Name: "ea-2"}},
			},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	reconcileAt(t, c, ss, t0)
	got := getStageSet(t, c, ns, "fail-preserve")
	if len(got.Status.Stages) != 2 {
		t.Fatalf("pass 1 should record both stages, got %+v", got.Status.Stages)
	}
	stage2Rev := stageStatusFor(got, "stage-2").AppliedRevision

	// Break stage-1's artifact (unparseable manifest) → pass 2 fails stage-1.
	repointArtifact(t, c, ns, "ea-1", map[string]string{"cm.yaml": "{ this is not a manifest"})
	reconcileAt(t, c, getStageSet(t, c, ns, "fail-preserve"), t0.Add(time.Minute))

	failed := getStageSet(t, c, ns, "fail-preserve")
	s1 := stageStatusFor(failed, "stage-1")
	if s1 == nil || s1.Phase != stagesv1.StageFailed {
		t.Fatalf("stage-1 should be recorded failed, got %+v", failed.Status.Stages)
	}
	s2 := stageStatusFor(failed, "stage-2")
	if s2 == nil {
		t.Fatalf("stage-2's persisted status was erased by stage-1's failure: %+v", failed.Status.Stages)
	}
	if s2.AppliedRevision != stage2Rev {
		t.Errorf("stage-2 appliedRevision = %q, want the preserved %q", s2.AppliedRevision, stage2Rev)
	}
}

// The core replay scenario behind the merge: a hold at stage-1 erases stage-2's
// lastHandledPromotion; when the hold clears, the promote annotation — which the
// controller never removes — compares against an empty record and fires the
// manual gate's break-glass, promoting a NEW revision past stage-2 with no
// operator involved: stage-3 receives a revision no one approved. With the
// merge, the handled token survives the hold and stage-2 waits for a fresh
// promote, keeping stage-3 at the approved revision.
func TestReconcile_PromotionHold_StalePromoteTokenDoesNotReplay(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	servedArtifact(t, c, ns, "ea-1", "", map[string]string{"cm.yaml": cmValManifest(ns, "replay-1", "v1")})
	servedArtifact(t, c, ns, "ea-2", "", map[string]string{"cm.yaml": cmValManifest(ns, "replay-2", "v1")})
	servedArtifact(t, c, ns, "ea-3", "", map[string]string{"cm.yaml": cmValManifest(ns, "replay-3", "v1")})

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "replay"},
		Spec: stagesv1.StageSetSpec{
			Interval: metav1.Duration{Duration: time.Minute},
			Stages: []stagesv1.Stage{
				{Name: "stage-1", SourceRef: stagesv1.SourceReference{Name: "ea-1"}, Promotion: &stagesv1.StagePromotion{Soak: dur(time.Minute)}},
				{Name: "stage-2", SourceRef: stagesv1.SourceReference{Name: "ea-2"}, Promotion: &stagesv1.StagePromotion{RequireManualPromotion: true}},
				{Name: "stage-3", SourceRef: stagesv1.SourceReference{Name: "ea-3"}},
			},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	// Pass 1 (t0): stage-1 soaks v1 → hold.
	reconcileAt(t, c, ss, t0)
	// Pass 2 (t0+2m): soak elapsed → stage-1 promotes; stage-2 applies v1 and
	// awaits manual promotion; stage-3 is not reached.
	reconcileAt(t, c, getStageSet(t, c, ns, "replay"), t0.Add(2*time.Minute))
	if v := cmDataKey(t, c, ns, "replay-2"); v != "v1" {
		t.Fatalf("stage-2 should be applied at v1 before promotion, got %q", v)
	}
	if cmExists(t, c, ns, "replay-3") {
		t.Fatal("stage-3 must not apply while stage-2 awaits manual promotion")
	}

	// Operator promotes stage-2; the annotation stays on the object afterwards.
	cur := getStageSet(t, c, ns, "replay")
	if cur.Annotations == nil {
		cur.Annotations = map[string]string{}
	}
	cur.Annotations[promoteAnnotation] = "stage-2@go1"
	if err := c.Update(context.Background(), cur); err != nil {
		t.Fatalf("annotate promote: %v", err)
	}
	// Pass 3 (t0+4m): stage-2 promotes via the token, stage-3 applies v1; the
	// full pass records lastHandledPromotion.
	reconcileAt(t, c, getStageSet(t, c, ns, "replay"), t0.Add(4*time.Minute))
	synced := getStageSet(t, c, ns, "replay")
	if s2 := stageStatusFor(synced, "stage-2"); s2 == nil || s2.LastHandledPromotion != "go1" {
		t.Fatalf("stage-2 should record the handled promote token, got %+v", synced.Status.Stages)
	}
	if v := cmDataKey(t, c, ns, "replay-3"); v != "v1" {
		t.Fatalf("stage-3 should be at the approved v1, got %q", v)
	}

	// New revision everywhere → pass 4 (t0+6m) holds at stage-1 soaking v2.
	repointArtifact(t, c, ns, "ea-1", map[string]string{"cm.yaml": cmValManifest(ns, "replay-1", "v2")})
	repointArtifact(t, c, ns, "ea-2", map[string]string{"cm.yaml": cmValManifest(ns, "replay-2", "v2")})
	repointArtifact(t, c, ns, "ea-3", map[string]string{"cm.yaml": cmValManifest(ns, "replay-3", "v2")})
	reconcileAt(t, c, getStageSet(t, c, ns, "replay"), t0.Add(6*time.Minute))
	heldAt := getStageSet(t, c, ns, "replay")
	s2 := stageStatusFor(heldAt, "stage-2")
	if s2 == nil || s2.LastHandledPromotion != "go1" {
		t.Fatalf("the hold at stage-1 erased stage-2's handled promote token: %+v", heldAt.Status.Stages)
	}

	// Pass 5 (t0+8m): stage-1's soak elapsed → promotes; stage-2 applies v2 and
	// sees the STALE annotation stage-2@go1. It was already handled, so the
	// manual gate must hold — v2 must NOT flow through to stage-3 via a
	// replayed break-glass.
	reconcileAt(t, c, getStageSet(t, c, ns, "replay"), t0.Add(8*time.Minute))
	final := getStageSet(t, c, ns, "replay")
	if v := cmDataKey(t, c, ns, "replay-3"); v != "v1" {
		t.Fatalf("v2 flowed past stage-2's manual gate via a STALE promote token (replay); stage-3 ConfigMap = %q, want v1", v)
	}
	fs2 := stageStatusFor(final, "stage-2")
	if fs2 == nil || fs2.PromotionState == nil || fs2.PromotionState.Phase != stagesv1.PromotionAwaitingManual {
		t.Fatalf("stage-2 should await a FRESH manual promote for v2, got %+v", fs2)
	}

	// A genuinely fresh token still promotes: the guard must not overshoot.
	cur = getStageSet(t, c, ns, "replay")
	cur.Annotations[promoteAnnotation] = "stage-2@go2"
	if err := c.Update(context.Background(), cur); err != nil {
		t.Fatalf("fresh promote: %v", err)
	}
	reconcileAt(t, c, getStageSet(t, c, ns, "replay"), t0.Add(10*time.Minute))
	if v := cmDataKey(t, c, ns, "replay-3"); v != "v2" {
		t.Fatalf("a fresh promote token must still promote v2 through to stage-3, got %q", v)
	}
}
