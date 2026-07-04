// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"net/http"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/artifact"
)

// A PERMANENTLY denied gate read (the tenant SA lost pods read) must NOT be
// classified as a transient errGateUnevaluable: backoff can't heal it, and
// leaving the last-written Ready condition (often Ready=True from the prior
// successful pass) would present a wedged rollout as healthy. It must flip
// Ready=False/RBACDenied with an actionable message and requeue at the bounded
// permanent-retry interval so a granted verb self-heals — without rolling the
// healthy rollout back.
func TestReconcile_Promotion_PermanentGateDenial_SetsRBACDenied(t *testing.T) {
	cfg := envtestConfig(t)
	scheme := testScheme(t)
	base, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("NewWithWatch: %v", err)
	}
	ns := newNamespace(t, base)
	servedArtifact(t, base, ns, "ea", "", map[string]string{"cm.yaml": configMapManifest(ns, "gated")})

	var denyPods bool
	gateReader := interceptor.NewClient(base, interceptor.Funcs{
		List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*corev1.PodList); ok && denyPods {
				return apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", nil)
			}
			return cl.List(ctx, list, opts...)
		},
	})

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "gate-denied"},
		Spec: stagesv1.StageSetSpec{
			Interval:          metav1.Duration{Duration: time.Minute},
			RollbackOnFailure: true,
			Stages: []stagesv1.Stage{{
				Name:      "stage-a",
				SourceRef: stagesv1.SourceReference{Name: "ea"},
				Promotion: &stagesv1.StagePromotion{RestartGate: &stagesv1.RestartGate{
					Checks: []stagesv1.RestartCheck{{
						Name:        "api",
						Selector:    metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}},
						MaxRestarts: 5,
					}},
				}},
			}},
		},
	}
	if err := base.Create(context.Background(), ss); err != nil {
		t.Fatalf("create: %v", err)
	}
	rec := &capturingRecorder{}
	newReconciler := func() *StageSetReconciler {
		return &StageSetReconciler{
			Client:     base,
			APIReader:  gateReader,
			RESTMapper: base.RESTMapper(),
			Recorder:   rec,
			Fetcher:    &artifact.Fetcher{HTTPClient: http.DefaultClient, URLValidator: artifact.PermissiveHTTPURL, IPValidator: artifact.PermissiveIP},
		}
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "gate-denied"}}

	// Baseline: gate reads cleanly, the stage promotes, Ready=True.
	if _, err := driveReconcile(newReconciler(), req); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	if got := readyReason(getStageSet(t, base, ns, "gate-denied")); got != ReasonReady {
		t.Fatalf("baseline Ready reason = %q, want %q", got, ReasonReady)
	}

	// The gate read is now permanently Forbidden.
	denyPods = true
	res, rerr := newReconciler().Reconcile(context.Background(), req)
	if rerr != nil {
		t.Fatalf("a permanent gate denial must NOT return an error for backoff, got: %v", rerr)
	}
	if res.RequeueAfter != permanentRetryInterval {
		t.Errorf("RequeueAfter = %v, want the bounded permanentRetryInterval %v", res.RequeueAfter, permanentRetryInterval)
	}
	got := getStageSet(t, base, ns, "gate-denied")
	if r := readyReason(got); r != ReasonRBACDenied {
		t.Fatalf("Ready reason on a permanent gate denial = %q, want %q — a silent transient retry leaves a wedged rollout looking healthy", r, ReasonRBACDenied)
	}
	if rec.has(eventReasonRolledBack) {
		t.Fatal("a gate-read denial must not roll the healthy rollout back")
	}
}
