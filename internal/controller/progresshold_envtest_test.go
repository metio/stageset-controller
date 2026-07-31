// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/artifact"
)

// neverReadyManifest renders a Deployment. envtest runs no kubelet and no
// deployment controller, so it stays InProgress forever — never Failed, which
// is exactly the shape a workload still running its migrations has when a
// verify wait runs out of time.
func neverReadyManifest(ns, name string) string {
	return "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: " + name +
		"\n  namespace: " + ns +
		"\nspec:\n  replicas: 1\n  selector:\n    matchLabels:\n      app: " + name +
		"\n  template:\n    metadata:\n      labels:\n        app: " + name +
		"\n    spec:\n      containers:\n        - name: c\n          image: docker.io/library/busybox:latest\n"
}

func progressHoldStageSet(ns, name, onTimeout string) *stagesv1.StageSet {
	return &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: stagesv1.StageSetSpec{
			Interval:          metav1.Duration{Duration: time.Hour},
			RetryInterval:     &metav1.Duration{Duration: 90 * time.Second},
			Timeout:           &metav1.Duration{Duration: time.Second},
			OnTimeout:         onTimeout,
			RollbackOnFailure: true,
			Stages: []stagesv1.Stage{{
				Name:      "server",
				SourceRef: stagesv1.SourceReference{Name: "ea"},
			}},
		},
	}
}

// The default. A workload still coming up when the clock runs out is reported as
// its own reason, holds rather than rolls back, and re-checks on the operator's
// spec.retryInterval instead of workqueue backoff — the three properties a
// portal deploying slow workloads unattended depends on.
func TestReconcile_VerifyTimeout_HoldsAsProgressing(t *testing.T) {
	cfg := envtestConfig(t)
	scheme := testScheme(t)
	base, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("NewWithWatch: %v", err)
	}
	ns := newNamespace(t, base)
	servedArtifact(t, base, ns, "ea", "", map[string]string{"dep.yaml": neverReadyManifest(ns, "server")})
	if err := base.Create(context.Background(), progressHoldStageSet(ns, "slow", "")); err != nil {
		t.Fatalf("create: %v", err)
	}

	rec := &capturingRecorder{}
	r := &StageSetReconciler{
		Client: base, APIReader: base, RESTMapper: base.RESTMapper(), Recorder: rec,
		Fetcher: &artifact.Fetcher{HTTPClient: http.DefaultClient, URLValidator: artifact.PermissiveHTTPURL, IPValidator: artifact.PermissiveIP},
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "slow"}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("finalizer pass: %v", err)
	}
	res, err := r.Reconcile(context.Background(), req)
	// A hold is not an error: returning one would engage workqueue backoff and
	// take the cadence out of the operator's hands.
	if err != nil {
		t.Fatalf("a still-progressing stage must not surface as a reconcile error, got: %v", err)
	}
	if res.RequeueAfter != 90*time.Second {
		t.Errorf("RequeueAfter = %v, want spec.retryInterval (90s)", res.RequeueAfter)
	}
	ss := getStageSet(t, base, ns, "slow")
	if got := readyReason(ss); got != ReasonStageProgressing {
		t.Fatalf("Ready reason = %q, want %q — a slow first install must not read as an outage", got, ReasonStageProgressing)
	}
	if rec.has(eventReasonRolledBack) {
		t.Error("rolled back a stage that was still progressing; that is the mid-migration case onTimeout: Hold exists to avoid")
	}
	// The stage records as Verifying, not Failed, and says what it waited on.
	var stage *stagesv1.StageStatus
	for i := range ss.Status.Stages {
		if ss.Status.Stages[i].Name == "server" {
			stage = &ss.Status.Stages[i]
		}
	}
	if stage == nil {
		t.Fatal("no status recorded for the held stage")
	}
	if stage.Phase != stagesv1.StageVerifying {
		t.Errorf("stage phase = %q, want %q", stage.Phase, stagesv1.StageVerifying)
	}
	if !strings.Contains(stage.Message, "spec.timeout") {
		t.Errorf("stage message does not name the knob that bounds the wait: %q", stage.Message)
	}
}

// Opting in restores the old behaviour: the timeout counts as a failure, so the
// run reports StageFailed and rollbackOnFailure engages.
func TestReconcile_VerifyTimeout_RollbackOptIn(t *testing.T) {
	cfg := envtestConfig(t)
	scheme := testScheme(t)
	base, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("NewWithWatch: %v", err)
	}
	ns := newNamespace(t, base)
	servedArtifact(t, base, ns, "ea", "", map[string]string{"dep.yaml": neverReadyManifest(ns, "server")})
	if err := base.Create(context.Background(), progressHoldStageSet(ns, "strict", "Rollback")); err != nil {
		t.Fatalf("create: %v", err)
	}

	r := &StageSetReconciler{
		Client: base, APIReader: base, RESTMapper: base.RESTMapper(), Recorder: &capturingRecorder{},
		Fetcher: &artifact.Fetcher{HTTPClient: http.DefaultClient, URLValidator: artifact.PermissiveHTTPURL, IPValidator: artifact.PermissiveIP},
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "strict"}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("finalizer pass: %v", err)
	}
	_, _ = r.Reconcile(context.Background(), req)

	if got := readyReason(getStageSet(t, base, ns, "strict")); got != ReasonStageFailed {
		t.Errorf("Ready reason = %q, want %q under onTimeout: Rollback", got, ReasonStageFailed)
	}
}
