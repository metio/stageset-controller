// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestStageReadyGauge(t *testing.T) {
	StageReady.Reset()

	SetStageReady("ns", "app", "infra", true)
	SetStageReady("ns", "app", "web", false)

	if v := testutil.ToFloat64(StageReady.WithLabelValues("ns", "app", "infra")); v != 1 {
		t.Fatalf("infra gauge = %v, want 1", v)
	}
	if v := testutil.ToFloat64(StageReady.WithLabelValues("ns", "app", "web")); v != 0 {
		t.Fatalf("web gauge = %v, want 0", v)
	}

	// Re-setting flips the value in place.
	SetStageReady("ns", "app", "web", true)
	if v := testutil.ToFloat64(StageReady.WithLabelValues("ns", "app", "web")); v != 1 {
		t.Fatalf("web gauge after flip = %v, want 1", v)
	}
}

func TestDeleteStageReady(t *testing.T) {
	StageReady.Reset()

	SetStageReady("ns", "app", "infra", true)
	SetStageReady("ns", "app", "web", true)
	SetStageReady("ns", "other", "x", true) // a different StageSet must survive

	DeleteStageReady("ns", "app")

	if n := testutil.CollectAndCount(StageReady); n != 1 {
		t.Fatalf("series after delete = %d, want 1 (only ns/other/x)", n)
	}
	if v := testutil.ToFloat64(StageReady.WithLabelValues("ns", "other", "x")); v != 1 {
		t.Fatalf("unrelated StageSet gauge = %v, want 1", v)
	}
}
