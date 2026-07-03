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
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/artifact"
)

// TestReconcile_Promotion_TransientGateErrorDoesNotRollBack pins that a
// promotion gate which cannot be EVALUATED (a transient RBAC/apiserver error
// reading its watched pods) backs off and retries WITHOUT rolling the healthy
// rollout back. The stage applied successfully — the gate simply could not be
// read — so treating that read error as a stage failure and running
// attemptRollback under rollbackOnFailure would revert a perfectly good
// deployment on a momentary hiccup.
func TestReconcile_Promotion_TransientGateErrorDoesNotRollBack(t *testing.T) {
	cfg := envtestConfig(t)
	scheme := testScheme(t)
	base, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("NewWithWatch: %v", err)
	}
	ns := newNamespace(t, base)
	servedArtifact(t, base, ns, "ea", "", map[string]string{"cm.yaml": configMapManifest(ns, "gated")})

	// The restart gate reads its watched pods through the reconciler's APIReader
	// (see gateReader). Fault-inject only that reader's pod List — and only while
	// failPods is set — so every other operation uses the real envtest client.
	var failPods bool
	gateReaderClient := interceptor.NewClient(base, interceptor.Funcs{
		List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*corev1.PodList); ok && failPods {
				return apierrors.NewServiceUnavailable("transient apiserver hiccup")
			}
			return cl.List(ctx, list, opts...)
		},
	})

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "gate-transient"},
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
			APIReader:  gateReaderClient,
			RESTMapper: base.RESTMapper(),
			Recorder:   rec,
			Fetcher:    &artifact.Fetcher{HTTPClient: http.DefaultClient, URLValidator: artifact.PermissiveHTTPURL, IPValidator: artifact.PermissiveIP},
		}
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "gate-transient"}}

	// Baseline: the gate reads pods cleanly (none match → within tolerance), the
	// stage promotes, and rollbackOnFailure records a snapshot to revert to.
	if _, err := driveReconcile(newReconciler(), req); err != nil {
		t.Fatalf("baseline reconcile: %v", err)
	}
	if got := readyReason(getStageSet(t, base, ns, "gate-transient")); got != ReasonReady {
		t.Fatalf("baseline Ready reason = %q, want %q", got, ReasonReady)
	}

	// Now the restart gate's pod read fails transiently. The stage still applies
	// successfully; only the gate cannot be evaluated. This must back off and
	// retry — not roll the healthy rollout back.
	failPods = true
	_, rerr := newReconciler().Reconcile(context.Background(), req)
	if rerr == nil {
		t.Fatal("a transient restart-gate read error must surface as an error for backoff, not be absorbed by a rollback")
	}
	if rec.has(eventReasonRolledBack) {
		t.Fatal("a transient gate-read error must not trigger a rollback of the healthy rollout")
	}
	if got := readyReason(getStageSet(t, base, ns, "gate-transient")); got != ReasonReady {
		t.Fatalf("Ready reason after a transient gate error = %q, want it to stay %q (no rollback)", got, ReasonReady)
	}
}
