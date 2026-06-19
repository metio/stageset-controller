// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

// TestEmitReadyEvent_DedupsRepeatedSameReason pins the parity with the jaas
// operator: a Ready-condition event fires only when the status or reason
// changes, so a StageSet re-reconciling at its steady interval (or retrying a
// terminal failure every permanentRetryInterval) does not re-emit an identical
// event on every pass.
func TestEmitReadyEvent_DedupsRepeatedSameReason(t *testing.T) {
	rec := &capturingRecorder{}
	r := &StageSetReconciler{Recorder: rec}
	ss := &stagesv1.StageSet{}

	count := func(reason string) int {
		n := 0
		for _, e := range rec.events {
			if e.reason == reason {
				n++
			}
		}
		return n
	}

	// First entry into Ready (no prior condition) emits.
	r.emitReadyEvent(ss, nil, metav1.ConditionTrue, ReasonReady, "synced")
	if got := count(ReasonReady); got != 1 {
		t.Fatalf("first Ready transition must emit exactly one event, got %d", got)
	}

	// A reconcile that leaves Ready unchanged must NOT re-emit.
	prevReady := &metav1.Condition{Type: ConditionReady, Status: metav1.ConditionTrue, Reason: ReasonReady}
	r.emitReadyEvent(ss, prevReady, metav1.ConditionTrue, ReasonReady, "synced again")
	if got := count(ReasonReady); got != 1 {
		t.Fatalf("repeated same-reason Ready event must be deduped, got %d", got)
	}

	// A transition to a different reason emits.
	r.emitReadyEvent(ss, prevReady, metav1.ConditionFalse, ReasonInvalidSpec, "bad spec")
	if got := count(ReasonInvalidSpec); got != 1 {
		t.Fatalf("Ready reason transition must emit, got %d", got)
	}

	// Recovery back to Ready (status+reason both change) emits again.
	prevInvalid := &metav1.Condition{Type: ConditionReady, Status: metav1.ConditionFalse, Reason: ReasonInvalidSpec}
	r.emitReadyEvent(ss, prevInvalid, metav1.ConditionTrue, ReasonReady, "recovered")
	if got := count(ReasonReady); got != 2 {
		t.Fatalf("recovery transition must emit, got %d Ready events", got)
	}

	// The emitted Ready events are Normal; the failure event is Warning.
	for _, e := range rec.events {
		switch e.reason {
		case ReasonReady:
			if e.etype != "Normal" {
				t.Errorf("Ready event type = %q, want Normal", e.etype)
			}
		case ReasonInvalidSpec:
			if e.etype != "Warning" {
				t.Errorf("InvalidSpec event type = %q, want Warning", e.etype)
			}
		}
	}
}
