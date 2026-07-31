// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

func TestStageTimeout(t *testing.T) {
	dur := func(d time.Duration) *metav1.Duration { return &metav1.Duration{Duration: d} }

	cases := []struct {
		name string
		// The ladder, most specific first: readyChecks.timeout, the stage's own
		// timeout, the StageSet's, then defaultStageTimeout.
		checks, stage, spec *metav1.Duration
		want                time.Duration
	}{
		{"readyChecks wins over stage and spec", dur(20 * time.Minute), dur(time.Minute), dur(time.Hour), 20 * time.Minute},
		{"readyChecks alone", dur(20 * time.Minute), nil, nil, 20 * time.Minute},
		{"readyChecks zero falls to stage", dur(0), dur(90 * time.Second), dur(time.Hour), 90 * time.Second},
		{"readyChecks negative falls to stage", dur(-time.Second), dur(90 * time.Second), nil, 90 * time.Second},
		{"readyChecks zero all the way down uses default", dur(0), dur(0), dur(0), defaultStageTimeout},
		{"stage positive wins", nil, dur(30 * time.Second), dur(time.Hour), 30 * time.Second},
		{"stage zero falls to spec", nil, dur(0), dur(2 * time.Minute), 2 * time.Minute},
		{"stage negative falls to spec", nil, dur(-time.Second), dur(2 * time.Minute), 2 * time.Minute},
		{"spec zero falls to default", nil, nil, dur(0), defaultStageTimeout},
		{"all unset uses default", nil, nil, nil, defaultStageTimeout},
		// The load-bearing case: an explicit 0s must NOT mean "expire
		// immediately" (which fails every wait-enabled stage at once).
		{"stage and spec zero uses default", nil, dur(0), dur(0), defaultStageTimeout},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ss := &stagesv1.StageSet{Spec: stagesv1.StageSetSpec{Timeout: c.spec}}
			stage := &stagesv1.Stage{Timeout: c.stage}
			if c.checks != nil {
				stage.ReadyChecks = &stagesv1.ReadyChecks{Timeout: c.checks}
			}
			if got := stageTimeout(ss, stage); got != c.want {
				t.Errorf("stageTimeout = %s, want %s", got, c.want)
			}
		})
	}
}

// A stage with readyChecks but no timeout on them must not be read as "0s".
// The nil-Timeout-inside-a-non-nil-ReadyChecks path is the one a stage that
// only lists checks or exprs takes, which is most of them.
func TestStageTimeout_ReadyChecksWithoutTimeoutFallsThrough(t *testing.T) {
	t.Parallel()
	ss := &stagesv1.StageSet{Spec: stagesv1.StageSetSpec{Timeout: &metav1.Duration{Duration: 2 * time.Minute}}}
	stage := &stagesv1.Stage{ReadyChecks: &stagesv1.ReadyChecks{DisableWait: true}}
	if got := stageTimeout(ss, stage); got != 2*time.Minute {
		t.Errorf("stageTimeout = %s, want 2m (readyChecks without a timeout must fall through)", got)
	}
}

// The default is in force exactly when a workload is least able to be quick —
// the first install, migrating against an empty database — so it is pinned
// rather than left to drift with an edit.
func TestStageTimeout_DefaultIsFifteenMinutes(t *testing.T) {
	t.Parallel()
	if defaultStageTimeout != 15*time.Minute {
		t.Errorf("defaultStageTimeout = %s, want 15m", defaultStageTimeout)
	}
}

func TestStageOnTimeout(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		stage, spec string
		want        string
	}{
		{"both unset holds", "", "", "Hold"},
		{"spec applies to every stage", "", "Rollback", "Rollback"},
		{"stage overrides spec", "Hold", "Rollback", "Hold"},
		{"stage opts into rollback alone", "Rollback", "", "Rollback"},
		{"stage empty falls to spec", "", "Hold", "Hold"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ss := &stagesv1.StageSet{Spec: stagesv1.StageSetSpec{OnTimeout: c.spec}}
			stage := &stagesv1.Stage{OnTimeout: c.stage}
			if got := stageOnTimeout(ss, stage); got != c.want {
				t.Errorf("stageOnTimeout = %q, want %q", got, c.want)
			}
		})
	}
}
