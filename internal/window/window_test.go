// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package window

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func mt(s string) *metav1.Time { return &metav1.Time{Time: at(s)} }

func dur(d time.Duration) *metav1.Duration { return &metav1.Duration{Duration: d} }

func TestDecision_NoWindowsAlwaysAllowed(t *testing.T) {
	t.Parallel()
	allowed, next, err := Decision(nil, at("2026-06-13T12:00:00Z"))
	if err != nil || !allowed || !next.IsZero() {
		t.Fatalf("no windows: allowed=%v next=%v err=%v", allowed, next, err)
	}
}

func TestDecision_AbsoluteAllowAndDeny(t *testing.T) {
	t.Parallel()
	allow := stagesv1.UpdateWindow{Type: TypeAllow, From: mt("2020-01-01T00:00:00Z"), To: mt("2030-01-01T00:00:00Z")}
	deny := stagesv1.UpdateWindow{Type: TypeDeny, From: mt("2020-01-01T00:00:00Z"), To: mt("2030-01-01T00:00:00Z")}
	now := at("2025-06-01T00:00:00Z")

	if allowed, _, _ := Decision([]stagesv1.UpdateWindow{allow}, now); !allowed {
		t.Fatal("active allow should permit")
	}
	if allowed, _, _ := Decision([]stagesv1.UpdateWindow{deny}, now); allowed {
		t.Fatal("active deny should block")
	}
	// Deny precedence: active allow + active deny → blocked.
	if allowed, _, _ := Decision([]stagesv1.UpdateWindow{allow, deny}, now); allowed {
		t.Fatal("deny must take precedence over allow")
	}
}

func TestDecision_AllowDeclaredButInactiveBlocks(t *testing.T) {
	t.Parallel()
	allow := stagesv1.UpdateWindow{Type: TypeAllow, From: mt("2020-01-01T00:00:00Z"), To: mt("2021-01-01T00:00:00Z")}
	allowed, next, err := Decision([]stagesv1.UpdateWindow{allow}, at("2025-06-01T00:00:00Z"))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if allowed {
		t.Fatal("an allow window exists but is inactive → must block")
	}
	// The window is wholly in the past, so there is no future boundary.
	if !next.IsZero() {
		t.Fatalf("past window should have no next boundary, got %v", next)
	}
}

func TestDecision_RecurringCronWindow(t *testing.T) {
	t.Parallel()
	// 02:00 daily for 2h, UTC.
	w := stagesv1.UpdateWindow{Type: TypeAllow, Schedule: "0 2 * * *", Duration: dur(2 * time.Hour)}

	// Inside the window.
	allowed, next, err := Decision([]stagesv1.UpdateWindow{w}, at("2026-06-13T02:30:00Z"))
	if err != nil || !allowed {
		t.Fatalf("02:30 should be inside: allowed=%v err=%v", allowed, err)
	}
	if !next.Equal(at("2026-06-13T04:00:00Z")) {
		t.Fatalf("boundary should be window end 04:00, got %v", next)
	}

	// Outside the window → next start is tomorrow 02:00.
	allowed, next, err = Decision([]stagesv1.UpdateWindow{w}, at("2026-06-13T05:00:00Z"))
	if err != nil || allowed {
		t.Fatalf("05:00 should be outside: allowed=%v err=%v", allowed, err)
	}
	if !next.Equal(at("2026-06-14T02:00:00Z")) {
		t.Fatalf("next start should be tomorrow 02:00, got %v", next)
	}
}

func TestDecision_RecurringHonorsTimeZone(t *testing.T) {
	t.Parallel()
	// 02:00 in Berlin = 00:00 UTC (CEST, +2 in summer).
	w := stagesv1.UpdateWindow{Type: TypeAllow, Schedule: "0 2 * * *", Duration: dur(time.Hour), TimeZone: "Europe/Berlin"}
	allowed, _, err := Decision([]stagesv1.UpdateWindow{w}, at("2026-06-13T00:30:00Z"))
	if err != nil || !allowed {
		t.Fatalf("00:30 UTC = 02:30 Berlin should be inside: allowed=%v err=%v", allowed, err)
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		w       stagesv1.UpdateWindow
		wantErr bool
	}{
		{"recurring ok", stagesv1.UpdateWindow{Type: TypeAllow, Schedule: "0 2 * * *", Duration: dur(time.Hour)}, false},
		{"absolute ok", stagesv1.UpdateWindow{Type: TypeDeny, From: mt("2026-01-01T00:00:00Z"), To: mt("2026-01-02T00:00:00Z")}, false},
		{"both recurring and absolute", stagesv1.UpdateWindow{Type: TypeAllow, Schedule: "0 2 * * *", Duration: dur(time.Hour), From: mt("2026-01-01T00:00:00Z"), To: mt("2026-01-02T00:00:00Z")}, true},
		{"neither", stagesv1.UpdateWindow{Type: TypeAllow}, true},
		{"bad cron", stagesv1.UpdateWindow{Type: TypeAllow, Schedule: "not a cron", Duration: dur(time.Hour)}, true},
		{"bad tz", stagesv1.UpdateWindow{Type: TypeAllow, Schedule: "0 2 * * *", Duration: dur(time.Hour), TimeZone: "Mars/Olympus"}, true},
		{"absolute to before from", stagesv1.UpdateWindow{Type: TypeDeny, From: mt("2026-01-02T00:00:00Z"), To: mt("2026-01-01T00:00:00Z")}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := Validate([]stagesv1.UpdateWindow{tc.w}); (err != nil) != tc.wantErr {
				t.Fatalf("Validate err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}
