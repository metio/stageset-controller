// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

// unpack spreads a promotionDecision over the five values the gate tests
// assert on.
func (d promotionDecision) unpack() (bool, *stagesv1.PromotionState, time.Duration, string, bool) {
	return d.promoted, d.state, d.requeueAfter, d.handled, d.rollback
}

const (
	tokRev1 = "rev1@sha256:aaa"
	tokRev2 = "rev2@sha256:bbb"
	tokRev3 = "rev3@sha256:ccc"
)

var tokNow = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

// promoteSS builds a StageSet carrying the given promote annotation and, when
// boundRev is set, the promote-revision annotation.
func promoteSS(promote, boundRev string) *stagesv1.StageSet {
	ss := &stagesv1.StageSet{}
	ss.Spec.Interval = metav1.Duration{Duration: time.Minute}
	ss.Annotations = map[string]string{}
	if promote != "" {
		ss.Annotations[promoteAnnotation] = promote
	}
	if boundRev != "" {
		ss.Annotations[promoteRevisionAnnotation] = boundRev
	}
	return ss
}

func phaseAt(phase stagesv1.PromotionPhase, rev string) stagesv1.StageStatus {
	return stagesv1.StageStatus{
		Name:            "staging",
		Phase:           stagesv1.StageReady,
		AppliedRevision: rev,
		PromotionState:  &stagesv1.PromotionState{Phase: phase, Since: &metav1.Time{Time: tokNow.Add(-time.Minute)}},
	}
}

func abortedAt(applied, aborted string) stagesv1.StageStatus {
	s := phaseAt(stagesv1.PromotionBlocked, applied)
	s.PromotionState.AbortedRevision = aborted
	return s
}

func TestJudgePromoteToken(t *testing.T) {
	gated := promoStage(&stagesv1.StagePromotion{RequireManualPromotion: true})
	ungated := promoStage(nil)

	cases := []struct {
		name       string
		ss         *stagesv1.StageSet
		stage      *stagesv1.Stage
		prior      stagesv1.StageStatus
		revision   string
		wantApply  string
		wantReason stagesv1.IgnoredPromotionReason
	}{
		{name: "no annotation", ss: promoteSS("", ""), stage: gated, prior: phaseAt(stagesv1.PromotionAwaitingManual, tokRev1), revision: tokRev1},
		{name: "malformed annotation without a token", ss: promoteSS("staging@", ""), stage: gated, prior: phaseAt(stagesv1.PromotionAwaitingManual, tokRev1), revision: tokRev1},
		{name: "malformed annotation without a separator", ss: promoteSS("staging", ""), stage: gated, prior: phaseAt(stagesv1.PromotionAwaitingManual, tokRev1), revision: tokRev1},
		{name: "token for another stage", ss: promoteSS("prod@t1", ""), stage: gated, prior: phaseAt(stagesv1.PromotionAwaitingManual, tokRev1), revision: tokRev1},
		{
			name: "token already handled", ss: promoteSS("staging@t1", ""), stage: gated, revision: tokRev1,
			prior: func() stagesv1.StageStatus {
				s := phaseAt(stagesv1.PromotionAwaitingManual, tokRev1)
				s.LastHandledPromotion = "t1"
				return s
			}(),
		},
		{
			name: "token already ignored", ss: promoteSS("staging@t1", ""), stage: gated, revision: tokRev1,
			prior: func() stagesv1.StageStatus {
				s := phaseAt(stagesv1.PromotionAwaitingManual, tokRev1)
				s.IgnoredPromotion = &stagesv1.IgnoredPromotion{Token: "t1", Reason: stagesv1.PromoteIgnoredAlreadyPromoted}
				return s
			}(),
		},
		{
			name: "a different ignored token does not shadow a fresh one", ss: promoteSS("staging@t2", ""), stage: gated, revision: tokRev1, wantApply: "t2",
			prior: func() stagesv1.StageStatus {
				s := phaseAt(stagesv1.PromotionAwaitingManual, tokRev1)
				s.IgnoredPromotion = &stagesv1.IgnoredPromotion{Token: "t1", Reason: stagesv1.PromoteIgnoredAlreadyPromoted}
				return s
			}(),
		},
		{name: "stage without a promotion gate", ss: promoteSS("staging@t1", ""), stage: ungated, prior: stagesv1.StageStatus{AppliedRevision: tokRev1}, revision: tokRev1, wantReason: stagesv1.PromoteIgnoredNoGate},
		{name: "stage already promoted at this revision", ss: promoteSS("staging@t1", ""), stage: gated, prior: phaseAt(stagesv1.PromotionPromoted, tokRev1), revision: tokRev1, wantReason: stagesv1.PromoteIgnoredAlreadyPromoted},
		{name: "stage promoted at an older revision", ss: promoteSS("staging@t1", ""), stage: gated, prior: phaseAt(stagesv1.PromotionPromoted, tokRev1), revision: tokRev2, wantReason: stagesv1.PromoteIgnoredNotHeld},
		{name: "rollout has not reached the stage", ss: promoteSS("staging@t1", ""), stage: gated, prior: stagesv1.StageStatus{}, revision: tokRev1, wantReason: stagesv1.PromoteIgnoredNotHeld},
		{name: "stage failed", ss: promoteSS("staging@t1", ""), stage: gated, prior: stagesv1.StageStatus{Phase: stagesv1.StageFailed, AppliedRevision: tokRev1}, revision: tokRev1, wantReason: stagesv1.PromoteIgnoredNotHeld},
		{name: "stage still verifying", ss: promoteSS("staging@t1", ""), stage: gated, prior: stagesv1.StageStatus{Phase: stagesv1.StageVerifying, AppliedRevision: tokRev1}, revision: tokRev1, wantReason: stagesv1.PromoteIgnoredNotHeld},
		{name: "stage held at an older revision", ss: promoteSS("staging@t1", ""), stage: gated, prior: phaseAt(stagesv1.PromotionSoaking, tokRev1), revision: tokRev2, wantReason: stagesv1.PromoteIgnoredNotHeld},
		{name: "promotion state without a phase", ss: promoteSS("staging@t1", ""), stage: gated, prior: phaseAt("", tokRev1), revision: tokRev1, wantReason: stagesv1.PromoteIgnoredNotHeld},
		{name: "soaking at this revision", ss: promoteSS("staging@t1", ""), stage: gated, prior: phaseAt(stagesv1.PromotionSoaking, tokRev1), revision: tokRev1, wantApply: "t1"},
		{name: "analyzing at this revision", ss: promoteSS("staging@t1", ""), stage: gated, prior: phaseAt(stagesv1.PromotionAnalyzing, tokRev1), revision: tokRev1, wantApply: "t1"},
		{name: "blocked at this revision", ss: promoteSS("staging@t1", ""), stage: gated, prior: phaseAt(stagesv1.PromotionBlocked, tokRev1), revision: tokRev1, wantApply: "t1"},
		{name: "awaiting manual at this revision", ss: promoteSS("staging@t1", ""), stage: gated, prior: phaseAt(stagesv1.PromotionAwaitingManual, tokRev1), revision: tokRev1, wantApply: "t1"},
		{name: "aborted at this revision", ss: promoteSS("staging@t1", ""), stage: gated, prior: abortedAt(tokRev1, tokRev2), revision: tokRev2, wantApply: "t1"},
		{name: "aborted, rolled out again at the last-good revision", ss: promoteSS("staging@t1", ""), stage: gated, prior: abortedAt(tokRev1, tokRev2), revision: tokRev1, wantReason: stagesv1.PromoteIgnoredNotHeld},
		{name: "aborted, a newer revision arrived", ss: promoteSS("staging@t1", ""), stage: gated, prior: abortedAt(tokRev1, tokRev2), revision: tokRev3, wantReason: stagesv1.PromoteIgnoredNotHeld},
		{name: "bound to the held revision", ss: promoteSS("staging@t1", tokRev1), stage: gated, prior: phaseAt(stagesv1.PromotionSoaking, tokRev1), revision: tokRev1, wantApply: "t1"},
		{name: "bound to another revision", ss: promoteSS("staging@t1", tokRev1), stage: gated, prior: phaseAt(stagesv1.PromotionSoaking, tokRev2), revision: tokRev2, wantReason: stagesv1.PromoteIgnoredRevisionMismatch},
		{name: "bound to the aborted revision", ss: promoteSS("staging@t1", tokRev2), stage: gated, prior: abortedAt(tokRev1, tokRev2), revision: tokRev2, wantApply: "t1"},
		{name: "bound to the last-good revision of an aborted stage", ss: promoteSS("staging@t1", tokRev1), stage: gated, prior: abortedAt(tokRev1, tokRev2), revision: tokRev2, wantReason: stagesv1.PromoteIgnoredRevisionMismatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := judgePromoteToken(tc.ss, tc.stage, tc.prior, tc.revision, tokNow)
			if v.apply != tc.wantApply {
				t.Fatalf("apply = %q, want %q", v.apply, tc.wantApply)
			}
			if tc.wantReason == "" {
				if v.ignored != nil {
					t.Fatalf("ignored = %+v, want none", v.ignored)
				}
				return
			}
			if v.apply != "" {
				t.Fatalf("a refused token must not apply, got %q", v.apply)
			}
			if v.ignored == nil {
				t.Fatalf("ignored = nil, want reason %s", tc.wantReason)
			}
			if v.ignored.Reason != tc.wantReason {
				t.Fatalf("reason = %s, want %s", v.ignored.Reason, tc.wantReason)
			}
			if v.ignored.Token != "t1" || v.ignored.Revision != tc.revision {
				t.Fatalf("ignored = %+v, want token t1 at revision %s", v.ignored, tc.revision)
			}
			if v.ignored.Time == nil || !v.ignored.Time.Time.Equal(tokNow) {
				t.Fatalf("ignored.time = %v, want %v", v.ignored.Time, tokNow)
			}
			if !strings.Contains(v.ignored.Message, `"t1"`) || !strings.Contains(v.ignored.Message, `"staging"`) {
				t.Fatalf("message %q should name the token and the stage", v.ignored.Message)
			}
		})
	}
}

func TestDescribeGateState(t *testing.T) {
	cases := []struct {
		prior stagesv1.StageStatus
		want  string
	}{
		{stagesv1.StageStatus{}, "the rollout had not reached it"},
		{stagesv1.StageStatus{Phase: stagesv1.StageFailed, AppliedRevision: tokRev1}, "it was Failed at revision \"" + tokRev1 + "\""},
		{phaseAt(stagesv1.PromotionPromoted, tokRev1), "it was Promoted at revision \"" + tokRev1 + "\""},
	}
	for _, tc := range cases {
		if got := describeGateState(tc.prior); got != tc.want {
			t.Errorf("describeGateState(%+v) = %q, want %q", tc.prior, got, tc.want)
		}
	}
}

// settle folds a gate decision into the stage status the controller would
// persist, so a test can feed it to the next evaluation as prior.
func settle(prior stagesv1.StageStatus, revision string, d promotionDecision) stagesv1.StageStatus {
	return stagesv1.StageStatus{
		Name:                 prior.Name,
		Phase:                stagesv1.StageReady,
		AppliedRevision:      revision,
		PromotionState:       d.state,
		LastHandledPromotion: d.handled,
		IgnoredPromotion:     d.ignoredPromotion(prior),
	}
}

// A token stamped on a stage that already cleared its gate is refused, and the
// next revision still waits at the gate instead of promoting at once.
func TestGatePromotion_TokenOnPromotedStageDoesNotCarryOver(t *testing.T) {
	r := &StageSetReconciler{Now: func() time.Time { return tokNow }}
	stage := promoStage(&stagesv1.StagePromotion{Soak: dur(10 * time.Minute), RequireManualPromotion: true})
	ss := promoteSS("staging@late", "")

	prior := phaseAt(stagesv1.PromotionPromoted, tokRev1)
	prior.LastHandledPromotion = "earlier"

	d := r.gatePromotion(ss, stage, tokRev1, prior, tokNow, nil, false, nil, nil)
	if !d.promoted || d.state.Phase != stagesv1.PromotionPromoted {
		t.Fatalf("a promoted stage stays promoted at its revision: %+v", d)
	}
	if d.handled != "earlier" {
		t.Fatalf("handled = %q, the refused token must not be recorded as handled", d.handled)
	}
	if d.ignored == nil || d.ignored.Reason != stagesv1.PromoteIgnoredAlreadyPromoted || d.ignored.Token != "late" {
		t.Fatalf("ignored = %+v, want token late refused as AlreadyPromoted", d.ignored)
	}
	prior = settle(prior, tokRev1, d)

	// The next revision arrives with the annotation still in place: the stage
	// soaks.
	d = r.gatePromotion(ss, stage, tokRev2, prior, tokNow, nil, false, nil, nil)
	if d.promoted || d.state.Phase != stagesv1.PromotionSoaking {
		t.Fatalf("revision 2 must soak, got promoted=%v state=%+v", d.promoted, d.state)
	}
	if d.ignored != nil {
		t.Fatalf("an already refused token is not judged again, got %+v", d.ignored)
	}
	prior = settle(prior, tokRev2, d)

	// After the soak it waits for the manual gate.
	later := tokNow.Add(11 * time.Minute)
	d = r.gatePromotion(ss, stage, tokRev2, prior, later, nil, false, nil, nil)
	if d.promoted || d.state.Phase != stagesv1.PromotionAwaitingManual {
		t.Fatalf("revision 2 must wait for the manual gate, got promoted=%v state=%+v", d.promoted, d.state)
	}
	prior = settle(prior, tokRev2, d)
	if prior.IgnoredPromotion == nil || prior.IgnoredPromotion.Token != "late" {
		t.Fatalf("the refusal must stay recorded, got %+v", prior.IgnoredPromotion)
	}

	// A fresh token for the held stage promotes it.
	d = r.gatePromotion(promoteSS("staging@ship", ""), stage, tokRev2, prior, later, nil, false, nil, nil)
	if !d.promoted || d.handled != "ship" || d.ignored != nil {
		t.Fatalf("a fresh token for a held stage must promote, got %+v", d)
	}
}

// The same carry-over must not skip a restart, event or analysis gate on the
// next revision.
func TestGatePromotion_RefusedTokenDoesNotOverrideBlockingGates(t *testing.T) {
	r := &StageSetReconciler{Now: func() time.Time { return tokNow }}
	ss := promoteSS("staging@late", "")
	prior := phaseAt(stagesv1.PromotionPromoted, tokRev1)

	t.Run("restart gate", func(t *testing.T) {
		d := r.gatePromotion(ss, promoStage(&stagesv1.StagePromotion{}), tokRev2, prior, tokNow, nil, false, &restartVerdict{check: "api", observed: 2}, nil)
		if d.promoted || d.state.Phase != stagesv1.PromotionBlocked {
			t.Fatalf("restart breach must block, got %+v", d)
		}
		if d.ignored == nil || d.ignored.Reason != stagesv1.PromoteIgnoredNotHeld {
			t.Fatalf("ignored = %+v, want NotHeld", d.ignored)
		}
	})
	t.Run("event gate", func(t *testing.T) {
		d := r.gatePromotion(ss, promoStage(&stagesv1.StagePromotion{}), tokRev2, prior, tokNow, nil, false, nil, &eventVerdict{check: "api", observed: 2})
		if d.promoted || d.state.Phase != stagesv1.PromotionBlocked {
			t.Fatalf("event breach must block, got %+v", d)
		}
	})
	t.Run("analysis", func(t *testing.T) {
		stage := promoStage(&stagesv1.StagePromotion{Analysis: &stagesv1.PromotionAnalysis{Checks: []stagesv1.AnalysisCheck{{Name: "err"}}}})
		d := r.gatePromotion(ss, stage, tokRev2, prior, tokNow, &analysisVerdict{breached: true}, false, nil, nil)
		if d.promoted {
			t.Fatalf("a failing analysis must hold, got %+v", d)
		}
	})
}

func TestGatePromotion_TokenForHeldStagePromotes(t *testing.T) {
	r := &StageSetReconciler{Now: func() time.Time { return tokNow }}
	stage := promoStage(&stagesv1.StagePromotion{Soak: dur(time.Hour)})

	t.Run("soaking at the current revision", func(t *testing.T) {
		d := r.gatePromotion(promoteSS("staging@go", ""), stage, tokRev1, phaseAt(stagesv1.PromotionSoaking, tokRev1), tokNow, nil, false, nil, nil)
		if !d.promoted || d.state.Phase != stagesv1.PromotionPromoted || d.handled != "go" || d.ignored != nil {
			t.Fatalf("got %+v, want promoted with token go handled", d)
		}
	})
	t.Run("soaking, bound to the current revision", func(t *testing.T) {
		d := r.gatePromotion(promoteSS("staging@go", tokRev1), stage, tokRev1, phaseAt(stagesv1.PromotionSoaking, tokRev1), tokNow, nil, false, nil, nil)
		if !d.promoted || d.handled != "go" {
			t.Fatalf("got %+v, want promoted", d)
		}
	})
	t.Run("blocked by a restart breach", func(t *testing.T) {
		d := r.gatePromotion(promoteSS("staging@go", ""), promoStage(&stagesv1.StagePromotion{}), tokRev1, phaseAt(stagesv1.PromotionBlocked, tokRev1), tokNow, nil, false, &restartVerdict{check: "api", observed: 1}, nil)
		if !d.promoted || d.handled != "go" {
			t.Fatalf("got %+v, want break-glass promotion", d)
		}
	})
}

// A token issued while the stage soaked at one revision does not promote the
// revision that replaced it before the controller saw the token.
func TestGatePromotion_TokenForSupersededRevisionIsRefused(t *testing.T) {
	r := &StageSetReconciler{Now: func() time.Time { return tokNow }}
	stage := promoStage(&stagesv1.StagePromotion{Soak: dur(time.Hour)})

	d := r.gatePromotion(promoteSS("staging@go", ""), stage, tokRev2, phaseAt(stagesv1.PromotionSoaking, tokRev1), tokNow, nil, false, nil, nil)
	if d.promoted || d.state.Phase != stagesv1.PromotionSoaking {
		t.Fatalf("the new revision must soak from scratch, got %+v", d)
	}
	if !d.state.Since.Time.Equal(tokNow) {
		t.Fatalf("soak since = %v, want a fresh start at %v", d.state.Since, tokNow)
	}
	if d.ignored == nil || d.ignored.Reason != stagesv1.PromoteIgnoredNotHeld || d.ignored.Revision != tokRev2 {
		t.Fatalf("ignored = %+v, want NotHeld at %s", d.ignored, tokRev2)
	}
}

// The race the promote-revision annotation closes: the stage already moved on
// to a newer revision and holds there, so the status-based check alone would
// accept a token the operator issued for the older one.
func TestGatePromotion_BoundTokenForOlderRevisionIsRefused(t *testing.T) {
	r := &StageSetReconciler{Now: func() time.Time { return tokNow }}
	stage := promoStage(&stagesv1.StagePromotion{Soak: dur(time.Hour)})

	d := r.gatePromotion(promoteSS("staging@go", tokRev1), stage, tokRev2, phaseAt(stagesv1.PromotionSoaking, tokRev2), tokNow, nil, false, nil, nil)
	if d.promoted || d.state.Phase != stagesv1.PromotionSoaking {
		t.Fatalf("the stage must keep soaking, got %+v", d)
	}
	if d.ignored == nil || d.ignored.Reason != stagesv1.PromoteIgnoredRevisionMismatch {
		t.Fatalf("ignored = %+v, want RevisionMismatch", d.ignored)
	}
	if !strings.Contains(d.ignored.Message, tokRev1) || !strings.Contains(d.ignored.Message, tokRev2) {
		t.Fatalf("message %q should name both revisions", d.ignored.Message)
	}
}

// A token stamped before the rollout reached the stage is refused when it
// gets there.
func TestGatePromotion_TokenBeforeStageReachedIsRefused(t *testing.T) {
	r := &StageSetReconciler{Now: func() time.Time { return tokNow }}
	stage := promoStage(&stagesv1.StagePromotion{RequireManualPromotion: true})

	d := r.gatePromotion(promoteSS("staging@early", ""), stage, tokRev1, stagesv1.StageStatus{}, tokNow, nil, false, nil, nil)
	if d.promoted || d.state.Phase != stagesv1.PromotionAwaitingManual {
		t.Fatalf("the stage must wait for the manual gate, got %+v", d)
	}
	if d.ignored == nil || d.ignored.Reason != stagesv1.PromoteIgnoredNotHeld {
		t.Fatalf("ignored = %+v, want NotHeld", d.ignored)
	}
}

func TestGatePromotion_TokenOnUngatedStageIsRefused(t *testing.T) {
	r := &StageSetReconciler{Now: func() time.Time { return tokNow }}
	prior := stagesv1.StageStatus{AppliedRevision: tokRev1, LastHandledPromotion: "old"}

	d := r.gatePromotion(promoteSS("staging@t", ""), promoStage(nil), tokRev1, prior, tokNow, nil, false, nil, nil)
	if !d.promoted || d.state != nil || d.handled != "old" {
		t.Fatalf("an ungated stage advances and keeps its handled token, got %+v", d)
	}
	if d.ignored == nil || d.ignored.Reason != stagesv1.PromoteIgnoredNoGate {
		t.Fatalf("ignored = %+v, want NoPromotionGate", d.ignored)
	}
	// Adding a gate later must not let the refused token fire.
	gated := promoStage(&stagesv1.StagePromotion{RequireManualPromotion: true})
	d = r.gatePromotion(promoteSS("staging@t", ""), gated, tokRev2, settle(prior, tokRev1, d), tokNow, nil, false, nil, nil)
	if d.promoted || d.ignored != nil {
		t.Fatalf("a refused token must stay refused once a gate exists, got %+v", d)
	}
}

// After an onFailure=Rollback abort the token refused while the stage was
// promoted stays refused, and does not promote the following revision.
func TestGatePromotion_TokenDoesNotApplyAfterAbort(t *testing.T) {
	r := &StageSetReconciler{Now: func() time.Time { return tokNow }}
	stage := promoStage(&stagesv1.StagePromotion{RestartGate: &stagesv1.RestartGate{
		OnFailure: "Rollback",
		Checks:    []stagesv1.RestartCheck{{Name: "api"}},
	}, RequireManualPromotion: true})
	ss := promoteSS("staging@late", "")

	// Promoted at rev1; the token is stamped and revision 2 breaches its
	// restart check, which reverts it.
	prior := phaseAt(stagesv1.PromotionPromoted, tokRev1)
	d := r.gatePromotion(ss, stage, tokRev2, prior, tokNow, nil, false, &restartVerdict{check: "api", observed: 1, rollback: true}, nil)
	if d.promoted || !d.rollback {
		t.Fatalf("revision 2 must roll back, got %+v", d)
	}
	if d.ignored == nil || d.ignored.Reason != stagesv1.PromoteIgnoredNotHeld {
		t.Fatalf("ignored = %+v, want NotHeld", d.ignored)
	}
	aborted := settle(prior, tokRev1, d)
	aborted.PromotionState.AbortedRevision = tokRev2
	if !rollbackAborted(ss, stage, aborted, tokRev2) {
		t.Fatal("the refused token must not un-park the aborted revision")
	}

	// Revision 3 arrives healthy: it waits for the manual gate.
	d = r.gatePromotion(ss, stage, tokRev3, aborted, tokNow, nil, false, nil, nil)
	if d.promoted || d.state.Phase != stagesv1.PromotionAwaitingManual {
		t.Fatalf("revision 3 must wait for the manual gate, got %+v", d)
	}
}

// A token consumed at one revision is not re-armed when a stage failure
// rewrites the stage status without a gate verdict.
func TestGatePromotion_TokenDoesNotApplyAfterStageFailure(t *testing.T) {
	r := &StageSetReconciler{Now: func() time.Time { return tokNow }}
	stage := promoStage(&stagesv1.StagePromotion{RequireManualPromotion: true})
	ss := promoteSS("staging@used", "")

	held := phaseAt(stagesv1.PromotionAwaitingManual, tokRev1)
	d := r.gatePromotion(ss, stage, tokRev1, held, tokNow, nil, false, nil, nil)
	if !d.promoted || d.handled != "used" {
		t.Fatalf("the token must promote the held stage, got %+v", d)
	}
	promoted := settle(held, tokRev1, d)

	failed := carryPromoteTokens(stagesv1.StageStatus{Name: "staging", Phase: stagesv1.StageFailed, AppliedRevision: tokRev2}, promoted)
	d = r.gatePromotion(ss, stage, tokRev2, failed, tokNow, nil, false, nil, nil)
	if d.promoted || d.state.Phase != stagesv1.PromotionAwaitingManual {
		t.Fatalf("after a failure, revision 2 must wait for the manual gate, got %+v", d)
	}
	if d.ignored != nil {
		t.Fatalf("a consumed token is not judged again, got %+v", d.ignored)
	}

	// Without the carried bookkeeping the token would be fresh, and is still
	// refused because the failed stage is not held.
	bare := stagesv1.StageStatus{Name: "staging", Phase: stagesv1.StageFailed, AppliedRevision: tokRev2}
	d = r.gatePromotion(ss, stage, tokRev2, bare, tokNow, nil, false, nil, nil)
	if d.promoted || d.ignored == nil || d.ignored.Reason != stagesv1.PromoteIgnoredNotHeld {
		t.Fatalf("got %+v, want held with the token refused as NotHeld", d)
	}
}

func TestRollbackAborted_BoundToken(t *testing.T) {
	stage := promoStage(&stagesv1.StagePromotion{RestartGate: &stagesv1.RestartGate{
		OnFailure: "Rollback",
		Checks:    []stagesv1.RestartCheck{{Name: "api"}},
	}})
	prior := abortedAt(tokRev1, tokRev2)
	if rollbackAborted(promoteSS("staging@go", tokRev2), stage, prior, tokRev2) {
		t.Error("a token bound to the aborted revision must un-park it")
	}
	if !rollbackAborted(promoteSS("staging@go", tokRev1), stage, prior, tokRev2) {
		t.Error("a token bound to another revision must leave the stage parked")
	}
}

func TestPromotionDecision_IgnoredPromotion(t *testing.T) {
	earlier := &stagesv1.IgnoredPromotion{Token: "a"}
	prior := stagesv1.StageStatus{IgnoredPromotion: earlier}
	if got := (promotionDecision{}).ignoredPromotion(prior); got != earlier {
		t.Fatalf("without a new refusal the earlier one is kept, got %+v", got)
	}
	fresh := &stagesv1.IgnoredPromotion{Token: "b"}
	if got := (promotionDecision{ignored: fresh}).ignoredPromotion(prior); got != fresh {
		t.Fatalf("a new refusal replaces the earlier one, got %+v", got)
	}
}

func TestCarryPromoteTokens(t *testing.T) {
	ignored := &stagesv1.IgnoredPromotion{Token: "x"}
	prior := stagesv1.StageStatus{
		LastHandledPromotion: "h",
		IgnoredPromotion:     ignored,
		PromotionState:       &stagesv1.PromotionState{Phase: stagesv1.PromotionSoaking},
	}
	got := carryPromoteTokens(stagesv1.StageStatus{Phase: stagesv1.StageFailed}, prior)
	if got.LastHandledPromotion != "h" || got.IgnoredPromotion != ignored {
		t.Fatalf("token bookkeeping not carried: %+v", got)
	}
	if got.PromotionState != nil {
		t.Fatal("the promotion state must not be carried: a failed stage is not held")
	}
}

func TestEmitPromotionIgnored(t *testing.T) {
	rec := &capturingRecorder{}
	r := &StageSetReconciler{Recorder: rec}
	r.emitPromotionIgnored(&stagesv1.StageSet{}, &stagesv1.IgnoredPromotion{Token: "t", Reason: stagesv1.PromoteIgnoredNotHeld, Message: "refused"})
	if len(rec.events) != 1 {
		t.Fatalf("events = %+v, want one", rec.events)
	}
	if e := rec.events[0]; e.etype != "Warning" || e.reason != eventReasonPromotionIgnored || e.note != "refused" {
		t.Fatalf("event = %+v, want a Warning PromotionIgnored carrying the message", e)
	}
}
