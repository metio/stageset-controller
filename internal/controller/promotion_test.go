// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/event"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

func dur(d time.Duration) *metav1.Duration { return &metav1.Duration{Duration: d} }

func promoStage(p *stagesv1.StagePromotion) *stagesv1.Stage {
	return &stagesv1.Stage{Name: "staging", Promotion: p}
}

// gatePromotion is the heart of the promotion gate; this table walks every
// branch (no gate, soak holding/elapsed/restarting, manual awaiting/approved,
// break-glass past a soak, already-promoted short-circuit).
func TestGatePromotion(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	r := &StageSetReconciler{Now: func() time.Time { return now }}
	const rev = "sha256:abc"
	ssWith := func(promoteAnn string) *stagesv1.StageSet {
		ss := &stagesv1.StageSet{}
		ss.Spec.Interval = metav1.Duration{Duration: time.Minute}
		if promoteAnn != "" {
			ss.Annotations = map[string]string{promoteAnnotation: promoteAnn}
		}
		return ss
	}

	t.Run("no promotion gate advances", func(t *testing.T) {
		promoted, state, _, _ := r.gatePromotion(ssWith(""), promoStage(nil), rev, stagesv1.StageStatus{}, now)
		if !promoted || state != nil {
			t.Fatalf("promoted=%v state=%v, want true/nil", promoted, state)
		}
	})

	t.Run("soak holds on first sight", func(t *testing.T) {
		promoted, state, req, _ := r.gatePromotion(ssWith(""), promoStage(&stagesv1.StagePromotion{Soak: dur(10 * time.Minute)}), rev, stagesv1.StageStatus{}, now)
		if promoted {
			t.Fatal("want holding during soak")
		}
		if state.Phase != stagesv1.PromotionSoaking {
			t.Fatalf("phase=%s, want Soaking", state.Phase)
		}
		if !state.Since.Time.Equal(now) || !state.SoakUntil.Time.Equal(now.Add(10*time.Minute)) {
			t.Fatalf("since=%v soakUntil=%v", state.Since, state.SoakUntil)
		}
		if req <= 0 || req > 10*time.Minute {
			t.Fatalf("requeue=%v out of range", req)
		}
	})

	t.Run("soak continues from prior since (same revision)", func(t *testing.T) {
		started := now.Add(-9 * time.Minute)
		prior := stagesv1.StageStatus{
			AppliedRevision: rev,
			PromotionState:  &stagesv1.PromotionState{Phase: stagesv1.PromotionSoaking, Since: &metav1.Time{Time: started}},
		}
		promoted, state, _, _ := r.gatePromotion(ssWith(""), promoStage(&stagesv1.StagePromotion{Soak: dur(10 * time.Minute)}), rev, prior, now)
		if promoted {
			t.Fatal("9m into a 10m soak should still hold")
		}
		if !state.Since.Time.Equal(started) {
			t.Fatalf("soak restarted: since=%v want %v", state.Since.Time, started)
		}
	})

	t.Run("soak elapsed promotes", func(t *testing.T) {
		started := now.Add(-11 * time.Minute)
		prior := stagesv1.StageStatus{
			AppliedRevision: rev,
			PromotionState:  &stagesv1.PromotionState{Phase: stagesv1.PromotionSoaking, Since: &metav1.Time{Time: started}},
		}
		promoted, state, _, _ := r.gatePromotion(ssWith(""), promoStage(&stagesv1.StagePromotion{Soak: dur(10 * time.Minute)}), rev, prior, now)
		if !promoted || state.Phase != stagesv1.PromotionPromoted {
			t.Fatalf("promoted=%v phase=%v, want true/Promoted", promoted, state.Phase)
		}
	})

	t.Run("new revision restarts the soak", func(t *testing.T) {
		// Prior soak was on an old revision and had elapsed/promoted; a new
		// revision must soak again from scratch, not inherit the old clock.
		prior := stagesv1.StageStatus{
			AppliedRevision: "sha256:OLD",
			PromotionState:  &stagesv1.PromotionState{Phase: stagesv1.PromotionPromoted, Since: &metav1.Time{Time: now.Add(-1 * time.Hour)}},
		}
		promoted, state, _, _ := r.gatePromotion(ssWith(""), promoStage(&stagesv1.StagePromotion{Soak: dur(10 * time.Minute)}), rev, prior, now)
		if promoted {
			t.Fatal("a new revision must re-soak")
		}
		if !state.Since.Time.Equal(now) {
			t.Fatalf("since=%v, want now (fresh soak)", state.Since.Time)
		}
	})

	t.Run("already promoted at this revision short-circuits", func(t *testing.T) {
		prior := stagesv1.StageStatus{
			AppliedRevision: rev,
			PromotionState:  &stagesv1.PromotionState{Phase: stagesv1.PromotionPromoted},
		}
		promoted, _, _, _ := r.gatePromotion(ssWith(""), promoStage(&stagesv1.StagePromotion{Soak: dur(10 * time.Minute)}), rev, prior, now)
		if !promoted {
			t.Fatal("a stage already promoted at this revision must not re-soak")
		}
	})

	t.Run("manual gate awaits without a token", func(t *testing.T) {
		promoted, state, _, _ := r.gatePromotion(ssWith(""), promoStage(&stagesv1.StagePromotion{RequireManualPromotion: true}), rev, stagesv1.StageStatus{}, now)
		if promoted || state.Phase != stagesv1.PromotionAwaitingManual {
			t.Fatalf("promoted=%v phase=%v, want false/AwaitingManual", promoted, state.Phase)
		}
	})

	t.Run("manual gate promotes on a fresh token", func(t *testing.T) {
		promoted, state, _, handled := r.gatePromotion(ssWith("staging@tok1"), promoStage(&stagesv1.StagePromotion{RequireManualPromotion: true}), rev, stagesv1.StageStatus{}, now)
		if !promoted || state.Phase != stagesv1.PromotionPromoted || handled != "tok1" {
			t.Fatalf("promoted=%v phase=%v handled=%q, want true/Promoted/tok1", promoted, state.Phase, handled)
		}
	})

	t.Run("an already-handled token does not re-promote", func(t *testing.T) {
		prior := stagesv1.StageStatus{AppliedRevision: rev, LastHandledPromotion: "tok1"}
		promoted, state, _, _ := r.gatePromotion(ssWith("staging@tok1"), promoStage(&stagesv1.StagePromotion{RequireManualPromotion: true}), rev, prior, now)
		if promoted || state.Phase != stagesv1.PromotionAwaitingManual {
			t.Fatalf("a stale token must not re-promote: promoted=%v phase=%v", promoted, state.Phase)
		}
	})

	t.Run("a token for another stage is ignored", func(t *testing.T) {
		promoted, _, _, _ := r.gatePromotion(ssWith("prod@tok1"), promoStage(&stagesv1.StagePromotion{RequireManualPromotion: true}), rev, stagesv1.StageStatus{}, now)
		if promoted {
			t.Fatal("a promote token addressed to another stage must not promote this one")
		}
	})

	t.Run("promote breaks a soak early (break-glass)", func(t *testing.T) {
		prior := stagesv1.StageStatus{
			AppliedRevision: rev,
			PromotionState:  &stagesv1.PromotionState{Phase: stagesv1.PromotionSoaking, Since: &metav1.Time{Time: now}},
		}
		promoted, state, _, handled := r.gatePromotion(ssWith("staging@brk"), promoStage(&stagesv1.StagePromotion{Soak: dur(1 * time.Hour)}), rev, prior, now)
		if !promoted || state.Phase != stagesv1.PromotionPromoted || handled != "brk" {
			t.Fatalf("promote should break a soak: promoted=%v phase=%v handled=%q", promoted, state.Phase, handled)
		}
	})

	t.Run("soak then manual: holds for soak, then awaits manual", func(t *testing.T) {
		p := &stagesv1.StagePromotion{Soak: dur(10 * time.Minute), RequireManualPromotion: true}
		// Mid-soak → Soaking.
		prior := stagesv1.StageStatus{AppliedRevision: rev, PromotionState: &stagesv1.PromotionState{Phase: stagesv1.PromotionSoaking, Since: &metav1.Time{Time: now.Add(-1 * time.Minute)}}}
		if _, state, _, _ := r.gatePromotion(ssWith(""), promoStage(p), rev, prior, now); state.Phase != stagesv1.PromotionSoaking {
			t.Fatalf("phase=%s, want Soaking", state.Phase)
		}
		// Soak elapsed, no token → AwaitingManual.
		prior.PromotionState.Since = &metav1.Time{Time: now.Add(-11 * time.Minute)}
		if promoted, state, _, _ := r.gatePromotion(ssWith(""), promoStage(p), rev, prior, now); promoted || state.Phase != stagesv1.PromotionAwaitingManual {
			t.Fatalf("after soak: promoted=%v phase=%v, want false/AwaitingManual", promoted, state.Phase)
		}
	})
}

func TestParsePromote(t *testing.T) {
	mk := func(v string) *stagesv1.StageSet {
		ss := &stagesv1.StageSet{}
		if v != "" {
			ss.Annotations = map[string]string{promoteAnnotation: v}
		}
		return ss
	}
	cases := []struct{ in, wantStage, wantTok string }{
		{"", "", ""},
		{"staging@abc", "staging", "abc"},
		{"staging@a@b", "staging", "a@b"},
		{"noat", "", ""},
		{"@tok", "", ""},
		{"stage@", "", ""},
	}
	for _, c := range cases {
		s, tok := parsePromote(mk(c.in))
		if s != c.wantStage || tok != c.wantTok {
			t.Errorf("parsePromote(%q) = (%q,%q), want (%q,%q)", c.in, s, tok, c.wantStage, c.wantTok)
		}
	}
	if got := promoteTokenFor(mk("staging@abc"), "staging"); got != "abc" {
		t.Errorf("promoteTokenFor(staging) = %q, want abc", got)
	}
	if got := promoteTokenFor(mk("staging@abc"), "prod"); got != "" {
		t.Errorf("promoteTokenFor(prod) = %q, want empty", got)
	}
}

func TestPromoteRequestedPredicate(t *testing.T) {
	mk := func(v string) *stagesv1.StageSet {
		ss := &stagesv1.StageSet{}
		if v != "" {
			ss.Annotations = map[string]string{promoteAnnotation: v}
		}
		return ss
	}
	p := promoteRequestedPredicate{}
	if p.Update(event.UpdateEvent{ObjectOld: mk("staging@a"), ObjectNew: mk("staging@b")}) != true {
		t.Error("a changed promote annotation must wake the controller")
	}
	if p.Update(event.UpdateEvent{ObjectOld: mk("staging@a"), ObjectNew: mk("staging@a")}) != false {
		t.Error("an unchanged promote annotation must not wake the controller")
	}
	if p.Update(event.UpdateEvent{ObjectOld: nil, ObjectNew: mk("x@y")}) != false {
		t.Error("a nil object must not panic or fire")
	}
}
