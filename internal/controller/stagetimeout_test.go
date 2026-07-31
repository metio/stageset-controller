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
		name        string
		stage, spec *metav1.Duration
		want        time.Duration
	}{
		{"stage positive wins", dur(30 * time.Second), dur(time.Hour), 30 * time.Second},
		{"stage zero falls to spec", dur(0), dur(2 * time.Minute), 2 * time.Minute},
		{"stage negative falls to spec", dur(-time.Second), dur(2 * time.Minute), 2 * time.Minute},
		{"spec zero falls to default", nil, dur(0), defaultStageTimeout},
		{"both unset uses default", nil, nil, defaultStageTimeout},
		// The load-bearing case: an explicit 0s must NOT mean "expire
		// immediately" (which fails every wait-enabled stage at once).
		{"stage and spec zero uses default", dur(0), dur(0), defaultStageTimeout},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ss := &stagesv1.StageSet{Spec: stagesv1.StageSetSpec{Timeout: c.spec}}
			stage := &stagesv1.Stage{Timeout: c.stage}
			if got := stageTimeout(ss, stage); got != c.want {
				t.Errorf("stageTimeout = %s, want %s", got, c.want)
			}
		})
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
