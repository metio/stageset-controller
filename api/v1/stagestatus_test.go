// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package v1

import "testing"

func TestStageStatusHeldRevision(t *testing.T) {
	cases := []struct {
		name   string
		status StageStatus
		want   string
	}{
		{"no promotion state", StageStatus{AppliedRevision: "r1"}, ""},
		{"empty phase", StageStatus{AppliedRevision: "r1", PromotionState: &PromotionState{}}, ""},
		{"promoted", StageStatus{AppliedRevision: "r1", PromotionState: &PromotionState{Phase: PromotionPromoted}}, ""},
		{"soaking", StageStatus{AppliedRevision: "r1", PromotionState: &PromotionState{Phase: PromotionSoaking}}, "r1"},
		{"analyzing", StageStatus{AppliedRevision: "r1", PromotionState: &PromotionState{Phase: PromotionAnalyzing}}, "r1"},
		{"blocked", StageStatus{AppliedRevision: "r1", PromotionState: &PromotionState{Phase: PromotionBlocked}}, "r1"},
		{"awaiting manual", StageStatus{AppliedRevision: "r1", PromotionState: &PromotionState{Phase: PromotionAwaitingManual}}, "r1"},
		{"aborted", StageStatus{AppliedRevision: "r1", PromotionState: &PromotionState{Phase: PromotionBlocked, AbortedRevision: "r2"}}, "r2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.status.HeldRevision(); got != tc.want {
				t.Fatalf("HeldRevision() = %q, want %q", got, tc.want)
			}
		})
	}
}
