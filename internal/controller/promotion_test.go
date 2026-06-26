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
		promoted, state, _, _, _ := r.gatePromotion(ssWith(""), promoStage(nil), rev, stagesv1.StageStatus{}, now, nil, false)
		if !promoted || state != nil {
			t.Fatalf("promoted=%v state=%v, want true/nil", promoted, state)
		}
	})

	t.Run("soak holds on first sight", func(t *testing.T) {
		promoted, state, req, _, _ := r.gatePromotion(ssWith(""), promoStage(&stagesv1.StagePromotion{Soak: dur(10 * time.Minute)}), rev, stagesv1.StageStatus{}, now, nil, false)
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
		promoted, state, _, _, _ := r.gatePromotion(ssWith(""), promoStage(&stagesv1.StagePromotion{Soak: dur(10 * time.Minute)}), rev, prior, now, nil, false)
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
		promoted, state, _, _, _ := r.gatePromotion(ssWith(""), promoStage(&stagesv1.StagePromotion{Soak: dur(10 * time.Minute)}), rev, prior, now, nil, false)
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
		promoted, state, _, _, _ := r.gatePromotion(ssWith(""), promoStage(&stagesv1.StagePromotion{Soak: dur(10 * time.Minute)}), rev, prior, now, nil, false)
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
		promoted, _, _, _, _ := r.gatePromotion(ssWith(""), promoStage(&stagesv1.StagePromotion{Soak: dur(10 * time.Minute)}), rev, prior, now, nil, false)
		if !promoted {
			t.Fatal("a stage already promoted at this revision must not re-soak")
		}
	})

	t.Run("manual gate awaits without a token", func(t *testing.T) {
		promoted, state, _, _, _ := r.gatePromotion(ssWith(""), promoStage(&stagesv1.StagePromotion{RequireManualPromotion: true}), rev, stagesv1.StageStatus{}, now, nil, false)
		if promoted || state.Phase != stagesv1.PromotionAwaitingManual {
			t.Fatalf("promoted=%v phase=%v, want false/AwaitingManual", promoted, state.Phase)
		}
	})

	t.Run("manual gate promotes on a fresh token", func(t *testing.T) {
		promoted, state, _, handled, _ := r.gatePromotion(ssWith("staging@tok1"), promoStage(&stagesv1.StagePromotion{RequireManualPromotion: true}), rev, stagesv1.StageStatus{}, now, nil, false)
		if !promoted || state.Phase != stagesv1.PromotionPromoted || handled != "tok1" {
			t.Fatalf("promoted=%v phase=%v handled=%q, want true/Promoted/tok1", promoted, state.Phase, handled)
		}
	})

	t.Run("an already-handled token does not re-promote", func(t *testing.T) {
		prior := stagesv1.StageStatus{AppliedRevision: rev, LastHandledPromotion: "tok1"}
		promoted, state, _, _, _ := r.gatePromotion(ssWith("staging@tok1"), promoStage(&stagesv1.StagePromotion{RequireManualPromotion: true}), rev, prior, now, nil, false)
		if promoted || state.Phase != stagesv1.PromotionAwaitingManual {
			t.Fatalf("a stale token must not re-promote: promoted=%v phase=%v", promoted, state.Phase)
		}
	})

	t.Run("a token for another stage is ignored", func(t *testing.T) {
		promoted, _, _, _, _ := r.gatePromotion(ssWith("prod@tok1"), promoStage(&stagesv1.StagePromotion{RequireManualPromotion: true}), rev, stagesv1.StageStatus{}, now, nil, false)
		if promoted {
			t.Fatal("a promote token addressed to another stage must not promote this one")
		}
	})

	t.Run("promote breaks a soak early (break-glass)", func(t *testing.T) {
		prior := stagesv1.StageStatus{
			AppliedRevision: rev,
			PromotionState:  &stagesv1.PromotionState{Phase: stagesv1.PromotionSoaking, Since: &metav1.Time{Time: now}},
		}
		promoted, state, _, handled, _ := r.gatePromotion(ssWith("staging@brk"), promoStage(&stagesv1.StagePromotion{Soak: dur(1 * time.Hour)}), rev, prior, now, nil, false)
		if !promoted || state.Phase != stagesv1.PromotionPromoted || handled != "brk" {
			t.Fatalf("promote should break a soak: promoted=%v phase=%v handled=%q", promoted, state.Phase, handled)
		}
	})

	t.Run("soak then manual: holds for soak, then awaits manual", func(t *testing.T) {
		p := &stagesv1.StagePromotion{Soak: dur(10 * time.Minute), RequireManualPromotion: true}
		// Mid-soak → Soaking.
		prior := stagesv1.StageStatus{AppliedRevision: rev, PromotionState: &stagesv1.PromotionState{Phase: stagesv1.PromotionSoaking, Since: &metav1.Time{Time: now.Add(-1 * time.Minute)}}}
		if _, state, _, _, _ := r.gatePromotion(ssWith(""), promoStage(p), rev, prior, now, nil, false); state.Phase != stagesv1.PromotionSoaking {
			t.Fatalf("phase=%s, want Soaking", state.Phase)
		}
		// Soak elapsed, no token → AwaitingManual.
		prior.PromotionState.Since = &metav1.Time{Time: now.Add(-11 * time.Minute)}
		if promoted, state, _, _, _ := r.gatePromotion(ssWith(""), promoStage(p), rev, prior, now, nil, false); promoted || state.Phase != stagesv1.PromotionAwaitingManual {
			t.Fatalf("after soak: promoted=%v phase=%v, want false/AwaitingManual", promoted, state.Phase)
		}
	})
}

// TestGatePromotion_Analysis walks the analysis branches: passing promotes,
// a breach within failureLimit holds (Analyzing), exceeding the limit blocks (or
// signals rollback), dryRun never holds, and a source error follows
// onSourceError.
func TestGatePromotion_Analysis(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	r := &StageSetReconciler{Now: func() time.Time { return now }}
	const rev = "sha256:abc"
	ss := &stagesv1.StageSet{}
	ss.Spec.Interval = metav1.Duration{Duration: time.Minute}

	withAnalysis := func(an *stagesv1.PromotionAnalysis) *stagesv1.Stage {
		return promoStage(&stagesv1.StagePromotion{Analysis: an})
	}
	baseAnalysis := func() *stagesv1.PromotionAnalysis {
		return &stagesv1.PromotionAnalysis{Checks: []stagesv1.AnalysisCheck{{Name: "err"}}}
	}
	pass := &analysisVerdict{result: &stagesv1.AnalysisResult{Passed: true}}
	breach := &analysisVerdict{result: &stagesv1.AnalysisResult{}, breached: true}
	srcErr := &analysisVerdict{result: &stagesv1.AnalysisResult{}, sourceErr: true}

	t.Run("passing analysis promotes", func(t *testing.T) {
		promoted, state, _, _, rb := r.gatePromotion(ss, withAnalysis(baseAnalysis()), rev, stagesv1.StageStatus{}, now, pass, false)
		if !promoted || rb || state.Phase != stagesv1.PromotionPromoted {
			t.Fatalf("promoted=%v rollback=%v phase=%v, want promoted", promoted, rb, state.Phase)
		}
	})

	t.Run("breach beyond failureLimit blocks", func(t *testing.T) {
		// failureLimit 0 → the first breach exceeds it.
		promoted, state, _, _, rb := r.gatePromotion(ss, withAnalysis(baseAnalysis()), rev, stagesv1.StageStatus{}, now, breach, false)
		if promoted || rb || state.Phase != stagesv1.PromotionBlocked || state.AnalysisFailures != 1 {
			t.Fatalf("promoted=%v rollback=%v phase=%v failures=%d, want Blocked/1", promoted, rb, state.Phase, state.AnalysisFailures)
		}
	})

	t.Run("breach within failureLimit holds Analyzing and counts up", func(t *testing.T) {
		an := baseAnalysis()
		an.FailureLimit = func() *int32 { v := int32(2); return &v }()
		promoted, state, _, _, _ := r.gatePromotion(ss, withAnalysis(an), rev, stagesv1.StageStatus{}, now, breach, false)
		if promoted || state.Phase != stagesv1.PromotionAnalyzing || state.AnalysisFailures != 1 {
			t.Fatalf("phase=%v failures=%d, want Analyzing/1", state.Phase, state.AnalysisFailures)
		}
		// A second consecutive breach (prior count 1) reaches 2 — still within
		// the limit of 2 — so it keeps holding; a third would exceed it.
		prior := stagesv1.StageStatus{AppliedRevision: rev, PromotionState: &stagesv1.PromotionState{Phase: stagesv1.PromotionAnalyzing, AnalysisFailures: 2}}
		_, state, _, _, _ = r.gatePromotion(ss, withAnalysis(an), rev, prior, now, breach, false)
		if state.Phase != stagesv1.PromotionBlocked || state.AnalysisFailures != 3 {
			t.Fatalf("phase=%v failures=%d, want Blocked/3", state.Phase, state.AnalysisFailures)
		}
	})

	t.Run("onFailure=Rollback signals rollback", func(t *testing.T) {
		an := baseAnalysis()
		an.OnFailure = "Rollback"
		_, state, _, _, rb := r.gatePromotion(ss, withAnalysis(an), rev, stagesv1.StageStatus{}, now, breach, false)
		if !rb || state.Phase != stagesv1.PromotionBlocked {
			t.Fatalf("rollback=%v phase=%v, want rollback/Blocked", rb, state.Phase)
		}
	})

	t.Run("dryRun never holds", func(t *testing.T) {
		an := baseAnalysis()
		an.DryRun = true
		promoted, state, _, _, rb := r.gatePromotion(ss, withAnalysis(an), rev, stagesv1.StageStatus{}, now, breach, false)
		if !promoted || rb || state.Phase != stagesv1.PromotionPromoted {
			t.Fatalf("dryRun: promoted=%v rollback=%v phase=%v, want promoted", promoted, rb, state.Phase)
		}
		if state.AnalysisFailures != 1 || state.LastAnalysis == nil {
			t.Fatalf("dryRun should still record failures(%d)/lastAnalysis(%v)", state.AnalysisFailures, state.LastAnalysis)
		}
	})

	t.Run("source error holds by default (onSourceError=Hold)", func(t *testing.T) {
		promoted, state, _, _, _ := r.gatePromotion(ss, withAnalysis(baseAnalysis()), rev, stagesv1.StageStatus{}, now, srcErr, false)
		if promoted || state.Phase != stagesv1.PromotionBlocked {
			t.Fatalf("source error default: promoted=%v phase=%v, want Blocked", promoted, state.Phase)
		}
	})

	t.Run("source error with onSourceError=Allow promotes", func(t *testing.T) {
		an := baseAnalysis()
		an.OnSourceError = "Allow"
		promoted, _, _, _, _ := r.gatePromotion(ss, withAnalysis(an), rev, stagesv1.StageStatus{}, now, srcErr, false)
		if !promoted {
			t.Fatal("onSourceError=Allow should promote despite a source error")
		}
	})

	t.Run("soak holds even when analysis already passes", func(t *testing.T) {
		an := baseAnalysis()
		p := &stagesv1.StagePromotion{Soak: dur(10 * time.Minute), Analysis: an}
		prior := stagesv1.StageStatus{AppliedRevision: rev, PromotionState: &stagesv1.PromotionState{Phase: stagesv1.PromotionSoaking, Since: &metav1.Time{Time: now.Add(-1 * time.Minute)}}}
		promoted, state, _, _, _ := r.gatePromotion(ss, promoStage(p), rev, prior, now, pass, false)
		if promoted || state.Phase != stagesv1.PromotionSoaking {
			t.Fatalf("mid-soak with passing analysis: phase=%v, want Soaking", state.Phase)
		}
	})
}

// TestGatePromotion_FastTrack: a healthy burn-rate metric promotes before the
// full soak (after the minimum), an unhealthy one waits it out, and fast-track
// never extends the soak.
func TestGatePromotion_FastTrack(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	r := &StageSetReconciler{Now: func() time.Time { return now }}
	const rev = "sha256:abc"
	ss := &stagesv1.StageSet{}
	ss.Spec.Interval = metav1.Duration{Duration: time.Minute}
	stage := promoStage(&stagesv1.StagePromotion{
		Soak:      dur(10 * time.Minute),
		FastTrack: &stagesv1.FastTrack{After: dur(2 * time.Minute)},
	})
	soakingSince := func(d time.Duration) stagesv1.StageStatus {
		return stagesv1.StageStatus{AppliedRevision: rev, PromotionState: &stagesv1.PromotionState{Phase: stagesv1.PromotionSoaking, Since: &metav1.Time{Time: now.Add(d)}}}
	}

	t.Run("healthy after minimum soak promotes early", func(t *testing.T) {
		promoted, state, _, _, _ := r.gatePromotion(ss, stage, rev, soakingSince(-3*time.Minute), now, nil, true)
		if !promoted || state.Phase != stagesv1.PromotionPromoted {
			t.Fatalf("promoted=%v phase=%v, want early promotion", promoted, state.Phase)
		}
	})
	t.Run("healthy but before minimum soak keeps soaking", func(t *testing.T) {
		promoted, state, _, _, _ := r.gatePromotion(ss, stage, rev, soakingSince(-1*time.Minute), now, nil, true)
		if promoted || state.Phase != stagesv1.PromotionSoaking {
			t.Fatalf("promoted=%v phase=%v, want still Soaking (before `after`)", promoted, state.Phase)
		}
	})
	t.Run("unhealthy metric waits out the full soak", func(t *testing.T) {
		promoted, state, _, _, _ := r.gatePromotion(ss, stage, rev, soakingSince(-3*time.Minute), now, nil, false)
		if promoted || state.Phase != stagesv1.PromotionSoaking {
			t.Fatalf("promoted=%v phase=%v, want Soaking (fastTrackOK=false)", promoted, state.Phase)
		}
	})
	t.Run("full soak elapsed promotes regardless", func(t *testing.T) {
		promoted, _, _, _, _ := r.gatePromotion(ss, stage, rev, soakingSince(-11*time.Minute), now, nil, false)
		if !promoted {
			t.Fatal("a fully-elapsed soak promotes even without fast-track")
		}
	})
}

// rollbackAborted skips re-applying a revision that a prior onFailure=Rollback
// reverted, until the revision changes or a promote token arrives.
func TestRollbackAborted(t *testing.T) {
	const rev = "sha256:new"
	rollbackStage := func() *stagesv1.Stage {
		return promoStage(&stagesv1.StagePromotion{Analysis: &stagesv1.PromotionAnalysis{
			OnFailure: "Rollback",
			Checks:    []stagesv1.AnalysisCheck{{Name: "err"}},
		}})
	}
	blockedPrior := stagesv1.StageStatus{
		AppliedRevision: "sha256:old",
		PromotionState:  &stagesv1.PromotionState{Phase: stagesv1.PromotionBlocked, AbortedRevision: rev},
	}
	ss := &stagesv1.StageSet{}

	if !rollbackAborted(ss, rollbackStage(), blockedPrior, rev) {
		t.Error("a stage blocked-and-aborted at the pinned revision should be skipped")
	}
	// A new revision clears the abort.
	if rollbackAborted(ss, rollbackStage(), blockedPrior, "sha256:newer") {
		t.Error("a different revision must not be treated as aborted")
	}
	// A fresh promote token un-aborts the stage.
	ssPromote := &stagesv1.StageSet{}
	ssPromote.Annotations = map[string]string{promoteAnnotation: "staging@go"}
	if rollbackAborted(ssPromote, rollbackStage(), blockedPrior, rev) {
		t.Error("a fresh promote token should un-abort the stage")
	}
	// onFailure=Hold (not Rollback) never aborts/skips.
	holdStage := promoStage(&stagesv1.StagePromotion{Analysis: &stagesv1.PromotionAnalysis{Checks: []stagesv1.AnalysisCheck{{Name: "err"}}}})
	if rollbackAborted(ss, holdStage, blockedPrior, rev) {
		t.Error("onFailure=Hold must not skip re-apply")
	}
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
