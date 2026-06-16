// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

func TestEffectiveInterval(t *testing.T) {
	t.Parallel()
	r := &StageSetReconciler{DefaultInterval: 10 * time.Minute}

	// Omitted spec.interval falls back to --default-interval.
	if got := r.effectiveInterval(&stagesv1.StageSet{}); got != 10*time.Minute {
		t.Fatalf("omitted interval = %v, want 10m", got)
	}

	// An explicit spec.interval wins over the default.
	ss := &stagesv1.StageSet{Spec: stagesv1.StageSetSpec{Interval: metav1.Duration{Duration: 2 * time.Minute}}}
	if got := r.effectiveInterval(ss); got != 2*time.Minute {
		t.Fatalf("explicit interval = %v, want 2m", got)
	}
}

func TestRetryAndSteadyInterval_UseDefault(t *testing.T) {
	t.Parallel()
	r := &StageSetReconciler{DefaultInterval: 10 * time.Minute}

	// retryInterval falls back through effectiveInterval to the default.
	if got := r.retryInterval(&stagesv1.StageSet{}); got != 10*time.Minute {
		t.Fatalf("retryInterval default = %v, want 10m", got)
	}

	// steadyInterval uses the default, and a shorter driftDetectionInterval wins.
	if got := r.steadyInterval(&stagesv1.StageSet{}); got != 10*time.Minute {
		t.Fatalf("steadyInterval default = %v, want 10m", got)
	}
	drift := metav1.Duration{Duration: 2 * time.Minute}
	ss := &stagesv1.StageSet{Spec: stagesv1.StageSetSpec{DriftDetectionInterval: &drift}}
	if got := r.steadyInterval(ss); got != 2*time.Minute {
		t.Fatalf("steadyInterval with drift = %v, want 2m", got)
	}
}
