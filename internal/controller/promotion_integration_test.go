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

// A stage with requireManualPromotion applies, then holds the rollout
// (Ready=False, AwaitingPromotion) until the promote annotation advances it.
func TestReconcile_Promotion_ManualHoldsThenPromotes(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	servedArtifact(t, c, ns, "ea", "", map[string]string{"cm.yaml": configMapManifest(ns, "manual")})

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "manual-gate"},
		Spec: stagesv1.StageSetSpec{
			Interval: metav1.Duration{Duration: time.Minute},
			Stages: []stagesv1.Stage{{
				Name:      "stage-a",
				SourceRef: stagesv1.SourceReference{Name: "ea"},
				Promotion: &stagesv1.StagePromotion{RequireManualPromotion: true},
			}},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}

	now := tm("2026-01-01T00:00:00Z").Time
	reconcileAt(t, c, ss, now)

	// The stage is applied (the gate holds *advancement*, not the apply)...
	if !cmExists(t, c, ns, "manual") {
		t.Fatal("the stage's objects should be applied before the promotion gate holds")
	}
	got := getStageSet(t, c, ns, "manual-gate")
	if readyReason(got) != ReasonAwaitingPromotion {
		t.Fatalf("Ready reason = %q, want %q", readyReason(got), ReasonAwaitingPromotion)
	}
	if len(got.Status.Stages) != 1 || got.Status.Stages[0].PromotionState == nil ||
		got.Status.Stages[0].PromotionState.Phase != stagesv1.PromotionAwaitingManual {
		t.Fatalf("status.stages[0].promotionState = %+v, want AwaitingManual", got.Status.Stages)
	}

	// ...then a promote advances it and the rollout completes.
	got.Annotations = map[string]string{promoteAnnotation: "stage-a@tok1"}
	if err := c.Update(context.Background(), got); err != nil {
		t.Fatalf("stamp promote annotation: %v", err)
	}
	reconcileAt(t, c, got, now.Add(time.Minute))

	done := getStageSet(t, c, ns, "manual-gate")
	if readyReason(done) != ReasonReady {
		t.Fatalf("after promote, Ready reason = %q, want %q", readyReason(done), ReasonReady)
	}
	if done.Status.Stages[0].LastHandledPromotion != "tok1" {
		t.Fatalf("lastHandledPromotion = %q, want tok1", done.Status.Stages[0].LastHandledPromotion)
	}
}

// A stage with a soak holds the rollout until the soak window elapses, then
// promotes on its own — no operator action.
func TestReconcile_Promotion_SoakHoldsThenElapses(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	servedArtifact(t, c, ns, "ea", "", map[string]string{"cm.yaml": configMapManifest(ns, "soaked")})

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "soak-gate"},
		Spec: stagesv1.StageSetSpec{
			Interval: metav1.Duration{Duration: time.Minute},
			Stages: []stagesv1.Stage{{
				Name:      "stage-a",
				SourceRef: stagesv1.SourceReference{Name: "ea"},
				Promotion: &stagesv1.StagePromotion{Soak: &metav1.Duration{Duration: 10 * time.Minute}},
			}},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}

	start := tm("2026-01-01T00:00:00Z").Time
	reconcileAt(t, c, ss, start)

	if !cmExists(t, c, ns, "soaked") {
		t.Fatal("the stage should apply before soaking")
	}
	got := getStageSet(t, c, ns, "soak-gate")
	if readyReason(got) != ReasonSoaking {
		t.Fatalf("Ready reason = %q, want %q", readyReason(got), ReasonSoaking)
	}
	if got.Status.Stages[0].PromotionState == nil || got.Status.Stages[0].PromotionState.SoakUntil == nil {
		t.Fatalf("status.stages[0].promotionState should carry soakUntil: %+v", got.Status.Stages[0].PromotionState)
	}

	// Past the soak window, the rollout promotes itself.
	reconcileAt(t, c, got, start.Add(11*time.Minute))
	done := getStageSet(t, c, ns, "soak-gate")
	if readyReason(done) != ReasonReady {
		t.Fatalf("after the soak, Ready reason = %q, want %q", readyReason(done), ReasonReady)
	}
}
