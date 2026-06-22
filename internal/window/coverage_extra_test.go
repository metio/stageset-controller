// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package window

import (
	"testing"
	"time"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

// TestDecision_PropagatesWindowError covers the error path in Decision: a single
// malformed window must abort evaluation, returning allowed=false, a zero
// boundary, and the underlying error.
func TestDecision_PropagatesWindowError(t *testing.T) {
	t.Parallel()
	// Neither recurring nor absolute → evalWindow errors, and Decision propagates.
	bad := stagesv1.UpdateWindow{Type: TypeAllow}
	allowed, next, err := Decision([]stagesv1.UpdateWindow{bad}, at("2026-06-13T12:00:00Z"))
	if err == nil {
		t.Fatal("malformed window must produce an error")
	}
	if allowed {
		t.Fatal("error path must report not allowed")
	}
	if !next.IsZero() {
		t.Fatalf("error path must report a zero boundary, got %v", next)
	}
}

// TestDecision_ErrorAfterValidWindow proves the error short-circuits even when an
// earlier window in the slice is well-formed, so the first malformed window wins.
func TestDecision_ErrorAfterValidWindow(t *testing.T) {
	t.Parallel()
	good := stagesv1.UpdateWindow{Type: TypeAllow, Schedule: "0 2 * * *", Duration: dur(time.Hour)}
	bad := stagesv1.UpdateWindow{Type: TypeDeny} // neither recurring nor absolute
	if _, _, err := Decision([]stagesv1.UpdateWindow{good, bad}, at("2026-06-13T12:00:00Z")); err == nil {
		t.Fatal("a malformed window after a valid one must still error")
	}
}

// TestDecision_AbsoluteFutureWindowReportsStart covers evalAbsolute's now<from
// branch: a wholly-future absolute window is inactive and reports its start as the
// next boundary.
func TestDecision_AbsoluteFutureWindowReportsStart(t *testing.T) {
	t.Parallel()
	w := stagesv1.UpdateWindow{Type: TypeAllow, From: mt("2030-01-01T00:00:00Z"), To: mt("2030-01-02T00:00:00Z")}
	allowed, next, err := Decision([]stagesv1.UpdateWindow{w}, at("2026-06-13T12:00:00Z"))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if allowed {
		t.Fatal("a future allow window is inactive → must block")
	}
	if !next.Equal(at("2030-01-01T00:00:00Z")) {
		t.Fatalf("next boundary should be the window start, got %v", next)
	}
}

// TestValidate_AbsoluteMissingTo covers evalAbsolute's from!=nil/to==nil guard:
// an absolute window declaring only one bound is malformed.
func TestValidate_AbsoluteMissingTo(t *testing.T) {
	t.Parallel()
	onlyFrom := stagesv1.UpdateWindow{Type: TypeAllow, From: mt("2026-01-01T00:00:00Z")}
	if err := Validate([]stagesv1.UpdateWindow{onlyFrom}); err == nil {
		t.Fatal("absolute window with from but no to must be rejected")
	}
	onlyTo := stagesv1.UpdateWindow{Type: TypeAllow, To: mt("2026-01-02T00:00:00Z")}
	if err := Validate([]stagesv1.UpdateWindow{onlyTo}); err == nil {
		t.Fatal("absolute window with to but no from must be rejected")
	}
}

// TestValidate_RecurringMissingDuration covers evalRecurring's
// schedule==""||duration==nil guard, reached when only one of the recurring pair
// is set.
func TestValidate_RecurringMissingDuration(t *testing.T) {
	t.Parallel()
	onlySchedule := stagesv1.UpdateWindow{Type: TypeAllow, Schedule: "0 2 * * *"}
	if err := Validate([]stagesv1.UpdateWindow{onlySchedule}); err == nil {
		t.Fatal("recurring window with schedule but no duration must be rejected")
	}
	onlyDuration := stagesv1.UpdateWindow{Type: TypeAllow, Duration: dur(time.Hour)}
	if err := Validate([]stagesv1.UpdateWindow{onlyDuration}); err == nil {
		t.Fatal("recurring window with duration but no schedule must be rejected")
	}
}

// TestValidate_RecurringNonPositiveDuration covers evalRecurring's dur<=0 guard:
// a schedule paired with a zero or negative duration is malformed.
func TestValidate_RecurringNonPositiveDuration(t *testing.T) {
	t.Parallel()
	zero := stagesv1.UpdateWindow{Type: TypeAllow, Schedule: "0 2 * * *", Duration: dur(0)}
	if err := Validate([]stagesv1.UpdateWindow{zero}); err == nil {
		t.Fatal("recurring window with zero duration must be rejected")
	}
	negative := stagesv1.UpdateWindow{Type: TypeAllow, Schedule: "0 2 * * *", Duration: dur(-time.Hour)}
	if err := Validate([]stagesv1.UpdateWindow{negative}); err == nil {
		t.Fatal("recurring window with negative duration must be rejected")
	}
}
