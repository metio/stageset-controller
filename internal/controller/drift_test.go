// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/metrics"
)

func driftCount(t *testing.T, ns, name, stage string) float64 {
	t.Helper()
	return testutil.ToFloat64(metrics.DriftCorrectedTotal.WithLabelValues(ns, name, stage))
}

// An object mutated out-of-band and corrected on a steady-state reconcile (same
// revision as last applied) is reported as drift: the value is restored and the
// drift metric increments.
func TestReconcile_DriftCorrected(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	servedArtifact(t, c, ns, "art", "", map[string]string{"cm.yaml": configMapManifest(ns, "managed")})

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "drifter"},
		Spec: stagesv1.StageSetSpec{
			Interval: metav1.Duration{Duration: time.Minute},
			Stages:   []stagesv1.Stage{{Name: "stage-a", SourceRef: stagesv1.SourceReference{Name: "art"}}},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	reconcileOnce(t, c, ss) // first apply: not drift
	if n := driftCount(t, ns, "drifter", "stage-a"); n != 0 {
		t.Fatalf("first apply must not count as drift, got %v", n)
	}

	// Tamper with the managed object out-of-band.
	var cm corev1.ConfigMap
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "managed"}, &cm); err != nil {
		t.Fatalf("get managed ConfigMap: %v", err)
	}
	cm.Data["key"] = "tampered"
	if err := c.Update(context.Background(), &cm); err != nil {
		t.Fatalf("tamper update: %v", err)
	}

	reconcileOnce(t, c, ss) // steady-state reconcile: same revision, drift corrected

	if got := cmDataKey(t, c, ns, "managed"); got != "value" {
		t.Fatalf("drift should be corrected back to desired, got key=%q", got)
	}
	if n := driftCount(t, ns, "drifter", "stage-a"); n != 1 {
		t.Fatalf("drift metric = %v, want 1", n)
	}
}

// A content change (new artifact revision) is the expected rollout, not drift.
func TestReconcile_NewRevisionIsNotDrift(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	servedArtifact(t, c, ns, "art", "", map[string]string{"cm.yaml": immutableConfigMapManifest(ns, "rolling", "v1")})

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "roller"},
		Spec: stagesv1.StageSetSpec{
			Interval: metav1.Duration{Duration: time.Minute},
			Stages: []stagesv1.Stage{{
				Name:           "stage-a",
				SourceRef:      stagesv1.SourceReference{Name: "art"},
				ConflictPolicy: &stagesv1.ConflictPolicy{Rules: []stagesv1.ConflictRule{recreateRule("ConfigMap", "", false)}},
			}},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	reconcileOnce(t, c, ss)

	// New content → new revision → expected rollout.
	repointArtifact(t, c, ns, "art", map[string]string{"cm.yaml": immutableConfigMapManifest(ns, "rolling", "v2")})
	reconcileOnce(t, c, ss)

	if got := cmDataKey(t, c, ns, "rolling"); got != "v2" {
		t.Fatalf("new revision should have rolled out, got key=%q", got)
	}
	if n := driftCount(t, ns, "roller", "stage-a"); n != 0 {
		t.Fatalf("a new-revision rollout must not be counted as drift, got %v", n)
	}
}
