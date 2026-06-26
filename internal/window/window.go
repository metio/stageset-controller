// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

// Package window evaluates a StageSet's update windows: whether new revisions
// may roll out at a given time, and when that decision next changes (for
// requeue and status). Deny windows take precedence; if any Allow window is
// declared, updates require an active Allow and no active Deny.
package window

import (
	"fmt"
	"time"

	"github.com/robfig/cron/v3"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

// Window types.
const (
	TypeAllow = "Allow"
	TypeDeny  = "Deny"
)

// Decision reports whether updates are allowed at now and the next time the
// decision could change (zero when nothing further changes). No windows means
// always allowed.
func Decision(windows []stagesv1.UpdateWindow, now time.Time) (allowed bool, nextChange time.Time, err error) {
	if len(windows) == 0 {
		return true, time.Time{}, nil
	}
	allowed, earliest, err := decisionAt(windows, now)
	if err != nil {
		return false, time.Time{}, err
	}
	// nextChange must be the instant the AGGREGATE decision flips, not merely the
	// earliest window boundary: when windows overlap, a boundary can pass without
	// changing the decision (e.g. one Allow ends while another Allow still
	// covers). evalWindow only yields each window's NEXT boundary, so walk
	// forward boundary-by-boundary, re-evaluating the aggregate, until it flips.
	// Bounded so a pathological set can't loop forever — on the cap we fall back
	// to the earliest boundary, a harmless early re-check.
	cur := now
	for i := 0; i < 2*len(windows)+2; i++ {
		_, boundary, derr := decisionAt(windows, cur)
		if derr != nil {
			return false, time.Time{}, derr
		}
		if boundary.IsZero() {
			// No further boundaries: the decision never changes again.
			return allowed, time.Time{}, nil
		}
		flipped, _, derr := decisionAt(windows, boundary)
		if derr != nil {
			return false, time.Time{}, derr
		}
		if flipped != allowed {
			return allowed, boundary, nil
		}
		cur = boundary
	}
	return allowed, earliest, nil
}

// decisionAt evaluates the aggregate allow/deny decision at t and the earliest
// next window boundary after t (deny-overrides; an Allow set means updates
// happen only while an Allow is active and no Deny is).
func decisionAt(windows []stagesv1.UpdateWindow, t time.Time) (allowed bool, earliestBoundary time.Time, err error) {
	var denyActive, allowActive, hasAllow bool
	var next time.Time
	for i := range windows {
		w := &windows[i]
		active, boundary, werr := evalWindow(w, t)
		if werr != nil {
			return false, time.Time{}, werr
		}
		switch w.Type {
		case TypeDeny:
			denyActive = denyActive || active
		case TypeAllow:
			hasAllow = true
			allowActive = allowActive || active
		}
		if !boundary.IsZero() && (next.IsZero() || boundary.Before(next)) {
			next = boundary
		}
	}
	return !denyActive && (!hasAllow || allowActive), next, nil
}

// Validate reports the first malformed window (used by the admission webhook).
func Validate(windows []stagesv1.UpdateWindow) error {
	for i := range windows {
		if _, _, err := evalWindow(&windows[i], time.Now()); err != nil {
			return err
		}
	}
	return nil
}

// evalWindow reports whether the window is active at now and the next boundary
// (start when inactive, end when active) after now.
func evalWindow(w *stagesv1.UpdateWindow, now time.Time) (active bool, nextBoundary time.Time, err error) {
	recurring := w.Schedule != "" || w.Duration != nil
	absolute := w.From != nil || w.To != nil
	switch {
	case recurring && absolute:
		return false, time.Time{}, fmt.Errorf("update window is both recurring (schedule/duration) and absolute (from/to)")
	case absolute:
		return evalAbsolute(w, now)
	case recurring:
		return evalRecurring(w, now)
	default:
		return false, time.Time{}, fmt.Errorf("update window sets neither a schedule+duration nor from+to")
	}
}

func evalAbsolute(w *stagesv1.UpdateWindow, now time.Time) (bool, time.Time, error) {
	if w.From == nil || w.To == nil {
		return false, time.Time{}, fmt.Errorf("absolute update window requires both from and to")
	}
	from, to := w.From.Time, w.To.Time
	if !to.After(from) {
		return false, time.Time{}, fmt.Errorf("absolute update window to must be after from")
	}
	switch {
	case now.Before(from):
		return false, from, nil
	case now.Before(to):
		return true, to, nil
	default:
		return false, time.Time{}, nil // wholly in the past
	}
}

func evalRecurring(w *stagesv1.UpdateWindow, now time.Time) (bool, time.Time, error) {
	if w.Schedule == "" || w.Duration == nil {
		return false, time.Time{}, fmt.Errorf("recurring update window requires both schedule and duration")
	}
	loc := time.UTC
	if w.TimeZone != "" {
		l, err := time.LoadLocation(w.TimeZone)
		if err != nil {
			return false, time.Time{}, fmt.Errorf("timeZone %q: %w", w.TimeZone, err)
		}
		loc = l
	}
	sched, err := cron.ParseStandard(w.Schedule)
	if err != nil {
		return false, time.Time{}, fmt.Errorf("schedule %q: %w", w.Schedule, err)
	}
	dur := w.Duration.Duration
	if dur <= 0 {
		return false, time.Time{}, fmt.Errorf("recurring update window duration must be positive")
	}
	nowL := now.In(loc)
	// Any window start that covers now began in (now-dur, now]. cron's Next is
	// strictly-after, so Next(now-dur) is the FIRST such start.
	candidate := sched.Next(nowL.Add(-dur))
	if candidate.After(nowL) {
		return false, candidate, nil // inactive; boundary = next start
	}
	// Active. When windows overlap (duration > schedule interval) several starts
	// cover now; the window stays open until the LATEST covering start's end, not
	// the earliest. Walk forward to the latest start still <= now so the reported
	// boundary is the furthest covering window end (for non-overlapping windows
	// there is only one covering start and the loop doesn't advance).
	latest := candidate
	for {
		next := sched.Next(latest)
		if next.After(nowL) {
			break
		}
		latest = next
	}
	return true, latest.Add(dur), nil // active; boundary = furthest covering window end
}
