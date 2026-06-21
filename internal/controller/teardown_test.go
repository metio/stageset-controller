// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/metrics"
)

func TestTeardownFailure_ForceDropMetricCountsOnceAcrossFailedUpdate(t *testing.T) {
	now := time.Now()
	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:         "team-a",
			Name:              "ss-forcedrop-once",
			Finalizers:        []string{FinalizerName},
			DeletionTimestamp: &metav1.Time{Time: now.Add(-2 * time.Hour)},
		},
	}
	var updateCalls int
	c := fake.NewClientBuilder().WithScheme(watchScheme(t)).WithObjects(ss).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if _, ok := obj.(*stagesv1.StageSet); ok {
					updateCalls++
					if updateCalls == 1 {
						return apierrors.NewConflict(schema.GroupResource{Resource: "stagesets"}, ss.Name, errors.New("conflict"))
					}
				}
				return cl.Update(ctx, obj, opts...)
			},
		}).Build()
	rec := &capturingRecorder{}
	r := &StageSetReconciler{Client: c, Recorder: rec, MaxTeardownWait: time.Hour, Now: func() time.Time { return now }}
	cause := errors.New("dial target: no route to host")
	labels := []string{ss.Namespace, ss.Name}
	before := testutil.ToFloat64(metrics.TeardownForceDropTotal.WithLabelValues(labels...))

	// Round 1: the finalizer-removal Update fails → error returned, nothing emitted.
	if _, err := r.teardownFailure(context.Background(), ss, "delete", cause); err == nil {
		t.Fatal("expected the failed finalizer Update to surface as an error")
	}
	if mid := testutil.ToFloat64(metrics.TeardownForceDropTotal.WithLabelValues(labels...)); mid != before {
		t.Errorf("force-drop metric moved on a failed-Update reconcile: before=%v mid=%v", before, mid)
	}
	if rec.has("TeardownForced") {
		t.Error("TeardownForced event emitted despite the failed Update")
	}
	// Round 2: retry succeeds → emit exactly once.
	if _, err := r.teardownFailure(context.Background(), ss, "delete", cause); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if after := testutil.ToFloat64(metrics.TeardownForceDropTotal.WithLabelValues(labels...)); after-before != 1 {
		t.Errorf("force-drop metric delta = %v, want exactly 1 across the failed+successful Update", after-before)
	}
}

func TestTeardownTimedOut(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name      string
		deletedAt *time.Time
		maxWait   time.Duration
		want      bool
	}{
		{"not deleting", nil, time.Hour, false},
		{"within wait", ptrTime(now.Add(-30 * time.Minute)), time.Hour, false},
		{"past wait", ptrTime(now.Add(-2 * time.Hour)), time.Hour, true},
		{"zero wait falls back to default (1h), within", ptrTime(now.Add(-30 * time.Minute)), 0, false},
		{"zero wait falls back to default (1h), past", ptrTime(now.Add(-90 * time.Minute)), 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ss := &stagesv1.StageSet{}
			if tc.deletedAt != nil {
				ss.DeletionTimestamp = &metav1.Time{Time: *tc.deletedAt}
			}
			r := &StageSetReconciler{MaxTeardownWait: tc.maxWait, Now: func() time.Time { return now }}
			got, _ := r.teardownTimedOut(ss)
			if got != tc.want {
				t.Fatalf("teardownTimedOut = %v, want %v", got, tc.want)
			}
		})
	}
}

func ptrTime(t time.Time) *time.Time { return &t }

// A teardown failure within the wait window keeps the finalizer and returns the
// error so controller-runtime retries.
func TestTeardownFailure_WithinWait_RetriesAndKeepsFinalizer(t *testing.T) {
	now := time.Now()
	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:         "team-a",
			Name:              "ss",
			Finalizers:        []string{FinalizerName},
			DeletionTimestamp: &metav1.Time{Time: now.Add(-1 * time.Minute)},
		},
	}
	c := fake.NewClientBuilder().WithScheme(watchScheme(t)).WithObjects(ss).Build()
	rec := &capturingRecorder{}
	r := &StageSetReconciler{Client: c, Recorder: rec, MaxTeardownWait: time.Hour, Now: func() time.Time { return now }}

	cause := errors.New("dial target: connection refused")
	_, err := r.teardownFailure(context.Background(), ss, "delete stage \"s\" objects", cause)
	if !errors.Is(err, cause) {
		t.Fatalf("within-wait teardown failure should return the cause for backoff, got %v", err)
	}
	if !controllerutil.ContainsFinalizer(ss, FinalizerName) {
		t.Fatal("finalizer must stay while within the teardown wait")
	}
	if rec.has("TeardownForced") {
		t.Fatal("no force-drop event should fire within the wait window")
	}
}

// Past the wait window the finalizer is force-dropped with a Warning event, and
// the object's finalizer is cleared in the store.
func TestTeardownFailure_PastWait_ForceDrops(t *testing.T) {
	now := time.Now()
	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:         "team-a",
			Name:              "ss",
			Finalizers:        []string{FinalizerName},
			DeletionTimestamp: &metav1.Time{Time: now.Add(-2 * time.Hour)},
		},
	}
	c := fake.NewClientBuilder().WithScheme(watchScheme(t)).WithObjects(ss).Build()
	rec := &capturingRecorder{}
	r := &StageSetReconciler{Client: c, Recorder: rec, MaxTeardownWait: time.Hour, Now: func() time.Time { return now }}

	cause := errors.New("dial target: no route to host")
	if _, err := r.teardownFailure(context.Background(), ss, "delete stage \"s\" objects", cause); err != nil {
		t.Fatalf("past-wait force-drop should not return an error: %v", err)
	}
	if controllerutil.ContainsFinalizer(ss, FinalizerName) {
		t.Fatal("finalizer should be force-dropped past the teardown wait")
	}
	if !rec.has("TeardownForced") {
		t.Fatal("force-drop should emit a Warning TeardownForced event")
	}
	// The store copy must also have the finalizer cleared so the apiserver GCs it.
	var got stagesv1.StageSet
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "team-a", Name: "ss"}, &got); err == nil {
		if controllerutil.ContainsFinalizer(&got, FinalizerName) {
			t.Fatal("persisted object should have the finalizer removed")
		}
	}
}
