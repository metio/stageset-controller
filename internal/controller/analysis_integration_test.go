// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/metrics"
)

func analysisCheck(max string) *stagesv1.PromotionAnalysis {
	return &stagesv1.PromotionAnalysis{
		Checks: []stagesv1.AnalysisCheck{{
			Name:      "error-rate",
			Source:    promSource(),
			Threshold: stagesv1.Threshold{Max: &max},
		}},
	}
}

func twoStage(ns, name string, promo *stagesv1.StagePromotion) *stagesv1.StageSet {
	return &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: stagesv1.StageSetSpec{
			Interval: metav1.Duration{Duration: time.Minute},
			Stages: []stagesv1.Stage{
				{Name: "stage-a", SourceRef: stagesv1.SourceReference{Name: "ea-a"}, Promotion: promo},
				{Name: "stage-b", SourceRef: stagesv1.SourceReference{Name: "ea-b"}},
			},
		},
	}
}

// A passing analysis advances past the gated stage and completes the rollout.
func TestReconcile_Promotion_AnalysisPassesPromotes(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	servedArtifact(t, c, ns, "ea-a", "", map[string]string{"cm.yaml": configMapManifest(ns, "an-a")})
	servedArtifact(t, c, ns, "ea-b", "", map[string]string{"cm.yaml": configMapManifest(ns, "an-b")})

	ss := twoStage(ns, "analysis-pass", &stagesv1.StagePromotion{Analysis: analysisCheck("0.01")})
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	reconcileWithQuerier(t, c, ss, tm("2026-01-01T00:00:00Z").Time, &fakeQuerier{value: 0.005})

	if !cmExists(t, c, ns, "an-a") || !cmExists(t, c, ns, "an-b") {
		t.Fatal("a passing analysis should advance through both stages")
	}
	got := getStageSet(t, c, ns, "analysis-pass")
	if readyReason(got) != ReasonReady {
		t.Fatalf("Ready reason = %q, want %q", readyReason(got), ReasonReady)
	}
	if got.Status.Stages[0].PromotionState == nil || got.Status.Stages[0].PromotionState.LastAnalysis == nil ||
		!got.Status.Stages[0].PromotionState.LastAnalysis.Passed {
		t.Fatalf("status should record a passing analysis: %+v", got.Status.Stages[0].PromotionState)
	}
}

// A failing analysis blocks the rollout at the gated stage; later stages never run.
func TestReconcile_Promotion_AnalysisFailsBlocks(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	servedArtifact(t, c, ns, "ea-a", "", map[string]string{"cm.yaml": configMapManifest(ns, "blk-a")})
	servedArtifact(t, c, ns, "ea-b", "", map[string]string{"cm.yaml": configMapManifest(ns, "blk-b")})

	ss := twoStage(ns, "analysis-block", &stagesv1.StagePromotion{Analysis: analysisCheck("0.01")})
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	reconcileWithQuerier(t, c, ss, tm("2026-01-01T00:00:00Z").Time, &fakeQuerier{value: 0.5})

	if !cmExists(t, c, ns, "blk-a") {
		t.Fatal("the gated stage still applies before its analysis gate")
	}
	if cmExists(t, c, ns, "blk-b") {
		t.Fatal("a blocked analysis must not advance to later stages")
	}
	got := getStageSet(t, c, ns, "analysis-block")
	if readyReason(got) != ReasonPromotionBlocked {
		t.Fatalf("Ready reason = %q, want %q", readyReason(got), ReasonPromotionBlocked)
	}
	if got.Status.Stages[0].PromotionState == nil || got.Status.Stages[0].PromotionState.Phase != stagesv1.PromotionBlocked {
		t.Fatalf("status.stages[0].promotionState = %+v, want Blocked", got.Status.Stages[0].PromotionState)
	}
	if v := testutil.ToFloat64(metrics.StagePromotionBlocked.WithLabelValues(ns, "analysis-block", "stage-a")); v != 1 {
		t.Errorf("promotion_blocked gauge = %v, want 1", v)
	}
}

// A failureLimit tolerates transient breaches: the first breach holds (Analyzing),
// the one past the limit blocks.
func TestReconcile_Promotion_AnalysisFailureLimit(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	servedArtifact(t, c, ns, "ea-a", "", map[string]string{"cm.yaml": configMapManifest(ns, "lim-a")})
	servedArtifact(t, c, ns, "ea-b", "", map[string]string{"cm.yaml": configMapManifest(ns, "lim-b")})

	an := analysisCheck("0.01")
	an.FailureLimit = func() *int32 { v := int32(1); return &v }()
	ss := twoStage(ns, "analysis-limit", &stagesv1.StagePromotion{Analysis: an})
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	q := &fakeQuerier{value: 0.5} // always breaching
	start := tm("2026-01-01T00:00:00Z").Time

	// First breach: failures=1, within the limit of 1 → Analyzing, not blocked.
	reconcileWithQuerier(t, c, ss, start, q)
	got := getStageSet(t, c, ns, "analysis-limit")
	if readyReason(got) == ReasonPromotionBlocked {
		t.Fatal("the first breach within failureLimit must not block yet")
	}
	if got.Status.Stages[0].PromotionState.Phase != stagesv1.PromotionAnalyzing {
		t.Fatalf("phase = %q, want Analyzing", got.Status.Stages[0].PromotionState.Phase)
	}

	// Second breach: failures=2, exceeds the limit → Blocked.
	reconcileWithQuerier(t, c, got, start.Add(time.Minute), q)
	got = getStageSet(t, c, ns, "analysis-limit")
	if readyReason(got) != ReasonPromotionBlocked {
		t.Fatalf("after exceeding failureLimit, Ready reason = %q, want %q", readyReason(got), ReasonPromotionBlocked)
	}
}

// onFailure=Rollback reverts the gated stage to its last-good revision and parks
// the failing revision so it is not re-applied each reconcile.
func TestReconcile_Promotion_AnalysisRollbackReverts(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	servedArtifact(t, c, ns, "ea-a", "", map[string]string{"cm.yaml": cmValManifest(ns, "rbk", "v1")})

	an := analysisCheck("0.01")
	an.OnFailure = "Rollback"
	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "analysis-rollback"},
		Spec: stagesv1.StageSetSpec{
			Interval:          metav1.Duration{Duration: time.Minute},
			RollbackOnFailure: true, // records the last-good snapshot to revert to
			Stages: []stagesv1.Stage{
				{Name: "stage-a", SourceRef: stagesv1.SourceReference{Name: "ea-a"}, Promotion: &stagesv1.StagePromotion{Analysis: an}},
			},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	start := tm("2026-01-01T00:00:00Z").Time

	// Healthy analysis → v1 applies and promotes; the snapshot records v1.
	reconcileWithQuerier(t, c, ss, start, &fakeQuerier{value: 0.005})
	if cmDataKey(t, c, ns, "rbk") != "v1" {
		t.Fatal("initial deploy should apply v1")
	}

	// v2 rolls forward but its analysis fails → revert to v1, park v2.
	repointArtifact(t, c, ns, "ea-a", map[string]string{"cm.yaml": cmValManifest(ns, "rbk", "v2")})
	reconcileWithQuerier(t, c, getStageSet(t, c, ns, "analysis-rollback"), start.Add(time.Minute), &fakeQuerier{value: 0.5})

	got := getStageSet(t, c, ns, "analysis-rollback")
	if readyReason(got) != ReasonPromotionBlocked {
		t.Fatalf("Ready reason = %q, want %q", readyReason(got), ReasonPromotionBlocked)
	}
	if v := cmDataKey(t, c, ns, "rbk"); v != "v1" {
		t.Fatalf("analysis failure with onFailure=Rollback should revert to v1, got %q", v)
	}
	ps := got.Status.Stages[0].PromotionState
	if ps == nil || ps.Phase != stagesv1.PromotionBlocked || ps.AbortedRevision == "" {
		t.Fatalf("status should record a Blocked phase with an abortedRevision: %+v", ps)
	}

	// A re-reconcile at the same (still failing) revision must NOT re-apply v2 —
	// the stage stays reverted and the source is not even queried.
	q := &fakeQuerier{value: 0.5}
	reconcileWithQuerier(t, c, getStageSet(t, c, ns, "analysis-rollback"), start.Add(2*time.Minute), q)
	if v := cmDataKey(t, c, ns, "rbk"); v != "v1" {
		t.Fatalf("an aborted revision must not be re-applied; want v1, got %q", v)
	}
	if q.calls != 0 {
		t.Errorf("an aborted stage must not re-query its analysis, got %d calls", q.calls)
	}
}

// onFailure=Rollback with no recorded snapshot (rollbackOnFailure off) can't
// revert, so it falls back to holding the stage blocked.
func TestReconcile_Promotion_AnalysisRollbackWithoutSnapshotHolds(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	servedArtifact(t, c, ns, "ea-a", "", map[string]string{"cm.yaml": configMapManifest(ns, "nosnap")})

	an := analysisCheck("0.01")
	an.OnFailure = "Rollback"
	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "rollback-nosnap"},
		Spec: stagesv1.StageSetSpec{
			Interval: metav1.Duration{Duration: time.Minute},
			Stages: []stagesv1.Stage{
				{Name: "stage-a", SourceRef: stagesv1.SourceReference{Name: "ea-a"}, Promotion: &stagesv1.StagePromotion{Analysis: an}},
			},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	reconcileWithQuerier(t, c, ss, tm("2026-01-01T00:00:00Z").Time, &fakeQuerier{value: 0.5})

	got := getStageSet(t, c, ns, "rollback-nosnap")
	if readyReason(got) != ReasonPromotionBlocked {
		t.Fatalf("Ready reason = %q, want %q", readyReason(got), ReasonPromotionBlocked)
	}
	if got.Status.Stages[0].PromotionState.AbortedRevision != "" {
		t.Fatalf("with no snapshot to revert to, no revision is aborted: %+v", got.Status.Stages[0].PromotionState)
	}
}

// dryRun records the would-block analysis but advances the rollout.
func TestReconcile_Promotion_AnalysisDryRunProceeds(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	servedArtifact(t, c, ns, "ea-a", "", map[string]string{"cm.yaml": configMapManifest(ns, "dry-a")})
	servedArtifact(t, c, ns, "ea-b", "", map[string]string{"cm.yaml": configMapManifest(ns, "dry-b")})

	an := analysisCheck("0.01")
	an.DryRun = true
	ss := twoStage(ns, "analysis-dry", &stagesv1.StagePromotion{Analysis: an})
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	reconcileWithQuerier(t, c, ss, tm("2026-01-01T00:00:00Z").Time, &fakeQuerier{value: 0.5})

	if !cmExists(t, c, ns, "dry-b") {
		t.Fatal("dryRun analysis must not hold the rollout")
	}
	got := getStageSet(t, c, ns, "analysis-dry")
	if readyReason(got) != ReasonReady {
		t.Fatalf("Ready reason = %q, want %q", readyReason(got), ReasonReady)
	}
	if got.Status.Stages[0].PromotionState == nil || got.Status.Stages[0].PromotionState.LastAnalysis == nil ||
		got.Status.Stages[0].PromotionState.LastAnalysis.Passed {
		t.Fatalf("dryRun should record the failing analysis result: %+v", got.Status.Stages[0].PromotionState)
	}
}
