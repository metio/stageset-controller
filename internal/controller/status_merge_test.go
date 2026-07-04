// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"testing"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

// mergeStageStatuses must keep the persisted record of every spec stage the
// pass never reached — losing a later stage's lastHandledPromotion lets the
// never-removed promote annotation replay through its gates.
func TestMergeStageStatuses(t *testing.T) {
	t.Parallel()
	specOf := func(names ...string) *stagesv1.StageSet {
		ss := &stagesv1.StageSet{}
		for _, n := range names {
			ss.Spec.Stages = append(ss.Spec.Stages, stagesv1.Stage{Name: n})
		}
		return ss
	}
	entry := func(name, rev string) stagesv1.StageStatus {
		return stagesv1.StageStatus{Name: name, AppliedRevision: rev}
	}

	t.Run("unprocessed later stages keep their prior entries", func(t *testing.T) {
		t.Parallel()
		ss := specOf("a", "b", "c")
		ss.Status.Stages = []stagesv1.StageStatus{
			entry("a", "r1"),
			{Name: "b", AppliedRevision: "r1", LastHandledPromotion: "b@tok"},
			entry("c", "r1"),
		}
		got := mergeStageStatuses(ss, []stagesv1.StageStatus{entry("a", "r2")})
		if len(got) != 3 {
			t.Fatalf("merged %d entries, want 3: %+v", len(got), got)
		}
		if got[0].Name != "a" || got[0].AppliedRevision != "r2" {
			t.Errorf("processed entry must win: %+v", got[0])
		}
		if got[1].Name != "b" || got[1].LastHandledPromotion != "b@tok" {
			t.Errorf("stage b's prior entry (incl. lastHandledPromotion) must survive: %+v", got[1])
		}
		if got[2].Name != "c" || got[2].AppliedRevision != "r1" {
			t.Errorf("stage c's prior entry must survive: %+v", got[2])
		}
	})

	t.Run("no prior status yields only the processed entries", func(t *testing.T) {
		t.Parallel()
		ss := specOf("a", "b")
		got := mergeStageStatuses(ss, []stagesv1.StageStatus{entry("a", "r1")})
		if len(got) != 1 || got[0].Name != "a" {
			t.Fatalf("got %+v, want just stage a", got)
		}
	})

	t.Run("full pass is unchanged by the merge", func(t *testing.T) {
		t.Parallel()
		ss := specOf("a", "b")
		ss.Status.Stages = []stagesv1.StageStatus{entry("a", "r1"), entry("b", "r1")}
		got := mergeStageStatuses(ss, []stagesv1.StageStatus{entry("a", "r2"), entry("b", "r2")})
		if len(got) != 2 || got[0].AppliedRevision != "r2" || got[1].AppliedRevision != "r2" {
			t.Fatalf("full pass should be all-processed entries, got %+v", got)
		}
	})

	t.Run("entries for stages removed from the spec drop", func(t *testing.T) {
		t.Parallel()
		ss := specOf("a")
		ss.Status.Stages = []stagesv1.StageStatus{entry("a", "r1"), entry("gone", "r1")}
		got := mergeStageStatuses(ss, []stagesv1.StageStatus{entry("a", "r2")})
		if len(got) != 1 || got[0].Name != "a" {
			t.Fatalf("removed stage must not be carried, got %+v", got)
		}
	})

	t.Run("output follows spec order regardless of prior order", func(t *testing.T) {
		t.Parallel()
		ss := specOf("a", "b", "c")
		ss.Status.Stages = []stagesv1.StageStatus{entry("c", "r1"), entry("a", "r1"), entry("b", "r1")}
		got := mergeStageStatuses(ss, []stagesv1.StageStatus{entry("a", "r2")})
		if len(got) != 3 || got[0].Name != "a" || got[1].Name != "b" || got[2].Name != "c" {
			t.Fatalf("want spec order a,b,c; got %+v", got)
		}
	})

	t.Run("empty processed set keeps every prior spec-stage entry", func(t *testing.T) {
		t.Parallel()
		ss := specOf("a", "b")
		ss.Status.Stages = []stagesv1.StageStatus{entry("a", "r1"), entry("b", "r1")}
		got := mergeStageStatuses(ss, nil)
		if len(got) != 2 {
			t.Fatalf("prior entries must survive an empty pass, got %+v", got)
		}
	})
}

// abortCapable must recognize every config that can stamp AbortedRevision —
// analysis, restart gate, event gate, gate-level or per-check — and nothing
// else, or a gate-driven abort is re-applied on the next reconcile.
func TestAbortCapable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		p    *stagesv1.StagePromotion
		want bool
	}{
		{"nil promotion", nil, false},
		{"empty promotion", &stagesv1.StagePromotion{}, false},
		{"analysis rollback", &stagesv1.StagePromotion{Analysis: &stagesv1.PromotionAnalysis{OnFailure: "Rollback"}}, true},
		{"analysis hold", &stagesv1.StagePromotion{Analysis: &stagesv1.PromotionAnalysis{OnFailure: "Hold"}}, false},
		{"analysis rollback dryRun", &stagesv1.StagePromotion{Analysis: &stagesv1.PromotionAnalysis{OnFailure: "Rollback", DryRun: true}}, false},
		{"restart gate default rollback", &stagesv1.StagePromotion{RestartGate: &stagesv1.RestartGate{OnFailure: "Rollback", Checks: []stagesv1.RestartCheck{{Name: "api"}}}}, true},
		{"restart gate default hold", &stagesv1.StagePromotion{RestartGate: &stagesv1.RestartGate{Checks: []stagesv1.RestartCheck{{Name: "api"}}}}, false},
		{"restart per-check rollback under gate hold", &stagesv1.StagePromotion{RestartGate: &stagesv1.RestartGate{Checks: []stagesv1.RestartCheck{{Name: "api"}, {Name: "worker", OnFailure: "Rollback"}}}}, true},
		{"event gate default rollback", &stagesv1.StagePromotion{EventGate: &stagesv1.EventGate{OnFailure: "Rollback", Checks: []stagesv1.EventCheck{{Name: "api"}}}}, true},
		{"event per-check rollback under gate hold", &stagesv1.StagePromotion{EventGate: &stagesv1.EventGate{Checks: []stagesv1.EventCheck{{Name: "api", OnFailure: "Rollback"}}}}, true},
		{"event gate hold everywhere", &stagesv1.StagePromotion{EventGate: &stagesv1.EventGate{Checks: []stagesv1.EventCheck{{Name: "api"}}}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := abortCapable(tc.p); got != tc.want {
				t.Fatalf("abortCapable = %v, want %v", got, tc.want)
			}
		})
	}
}
