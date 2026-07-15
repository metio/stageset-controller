// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fluxcd/pkg/apis/meta"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/actions"
	"github.com/metio/stageset-controller/internal/artifact"
)

// onRollbackReconciler builds a reconciler wired for onRollback action tests: a
// permissive artifact fetcher (loopback httptest sources), a capturing recorder
// so the best-effort Warning event can be asserted, and the action host/IP
// allowances an http action needs to reach a loopback test server.
func onRollbackReconciler(c client.Client, rec *capturingRecorder, allowedHosts []string) *StageSetReconciler {
	r := &StageSetReconciler{
		Client:             c,
		RESTMapper:         c.RESTMapper(),
		Fetcher:            &artifact.Fetcher{HTTPClient: http.DefaultClient, URLValidator: artifact.PermissiveHTTPURL, IPValidator: artifact.PermissiveIP},
		AllowedActionHosts: allowedHosts,
		ActionIPValidator:  actions.PermissiveIP,
	}
	// Assign through the interface only for a real recorder: a typed-nil pointer
	// would make r.Recorder a non-nil interface and defeat the nil-guard in event.
	if rec != nil {
		r.Recorder = rec
	}
	return r
}

// deleteSentinel is an onRollback action that removes a marker ConfigMap — the
// observable stand-in for external cleanup (lifting a maintenance mode) that must
// run only once the previous manifests are restored.
func deleteSentinel(ns, name string) stagesv1.Action {
	return stagesv1.Action{
		Name:   "cleanup-" + name,
		Delete: &stagesv1.DeleteAction{Target: meta.NamespacedObjectKindReference{APIVersion: "v1", Kind: "ConfigMap", Name: name, Namespace: ns}},
	}
}

func createSentinel(t *testing.T, c client.Client, ns, name string) {
	t.Helper()
	if err := c.Create(context.Background(), &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}); err != nil {
		t.Fatalf("create sentinel %q: %v", name, err)
	}
}

func sentinelGone(t *testing.T, c client.Client, ns, name string) bool {
	t.Helper()
	var cm corev1.ConfigMap
	err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &cm)
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("get sentinel %q: %v", name, err)
	}
	return apierrors.IsNotFound(err)
}

// The no-snapshot guard: a first run that fails has nothing to roll back to, so
// onRollback must NOT fire — there is no restored state to clean up against.
func TestReconcile_OnRollback_NotFiredWithoutSnapshot(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	servedArtifact(t, c, ns, "ea", "", map[string]string{"cm.yaml": cmValManifest(ns, "Bad_Name", "x")})
	createSentinel(t, c, ns, "maintenance")

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "rb-nosnap"},
		Spec: stagesv1.StageSetSpec{
			Interval:          metav1.Duration{Duration: time.Minute},
			RollbackOnFailure: true,
			OnRollback:        []stagesv1.Action{deleteSentinel(ns, "maintenance")},
			Stages:            []stagesv1.Stage{{Name: "stage-a", SourceRef: stagesv1.SourceReference{Name: "ea"}}},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	_ = reconcileWith(t, c, ss, nil)

	if readyReason(getStageSet(t, c, ns, "rb-nosnap")) != ReasonStageFailed {
		t.Fatal("first run should fail")
	}
	if sentinelGone(t, c, ns, "maintenance") {
		t.Fatal("onRollback fired with no snapshot to restore; the sentinel should be untouched")
	}
}

// A successful run never rolls back, so onRollback never runs.
func TestReconcile_OnRollback_NotFiredOnSuccess(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	servedArtifact(t, c, ns, "ea", "", map[string]string{"cm.yaml": cmValManifest(ns, "ok", "v1")})
	createSentinel(t, c, ns, "maintenance")

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "rb-success"},
		Spec: stagesv1.StageSetSpec{
			Interval:          metav1.Duration{Duration: time.Minute},
			RollbackOnFailure: true,
			OnRollback:        []stagesv1.Action{deleteSentinel(ns, "maintenance")},
			Stages:            []stagesv1.Stage{{Name: "stage-a", SourceRef: stagesv1.SourceReference{Name: "ea"}}},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	reconcileOnce(t, c, ss)

	if got := readyReason(getStageSet(t, c, ns, "rb-success")); got != ReasonReady {
		t.Fatalf("run should be Ready, got %q", got)
	}
	if sentinelGone(t, c, ns, "maintenance") {
		t.Fatal("onRollback fired on a successful run; the sentinel should be untouched")
	}
}

// The second wiring site: a promotion gate's single-stage onFailure=Rollback
// revert also runs the StageSet-level onRollback cleanup once the stage's
// manifests are restored.
func TestReconcile_OnRollback_FiresOnGateRollback(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	servedArtifact(t, c, ns, "ea", "", map[string]string{"cm.yaml": cmValManifest(ns, "gated", "v1")})
	createSentinel(t, c, ns, "maintenance")

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "rb-gate"},
		Spec: stagesv1.StageSetSpec{
			Interval:          metav1.Duration{Duration: time.Minute},
			RollbackOnFailure: true,
			OnRollback:        []stagesv1.Action{deleteSentinel(ns, "maintenance")},
			Stages: []stagesv1.Stage{{
				Name:      "stage-a",
				SourceRef: stagesv1.SourceReference{Name: "ea"},
				Promotion: &stagesv1.StagePromotion{RestartGate: &stagesv1.RestartGate{
					OnFailure: "Rollback",
					Checks: []stagesv1.RestartCheck{{
						Name:        "api",
						Selector:    metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}},
						MaxRestarts: 0,
					}},
				}},
			}},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	rec := &capturingRecorder{}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "rb-gate"}}

	// Pass 1: no breaching pods, v1 promotes, snapshot records v1.
	if _, err := driveReconcile(onRollbackReconciler(c, rec, nil), req); err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	if got := readyReason(getStageSet(t, c, ns, "rb-gate")); got != ReasonReady {
		t.Fatalf("pass 1 Ready = %q, want Ready", got)
	}
	if sentinelGone(t, c, ns, "maintenance") {
		t.Fatal("onRollback must not fire on the clean first pass")
	}

	// v2 arrives while the watched pods crash-loop: the gate breaches and reverts.
	repointArtifact(t, c, ns, "ea", map[string]string{"cm.yaml": cmValManifest(ns, "gated", "v2")})
	breachingPod(t, c, ns, "api-crashy", 3)
	if _, err := driveReconcile(onRollbackReconciler(c, rec, nil), req); err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	if v := cmDataKey(t, c, ns, "gated"); v != "v1" {
		t.Fatalf("gate rollback should restore v1, got %q", v)
	}
	if !sentinelGone(t, c, ns, "maintenance") {
		t.Fatal("onRollback should have run after the gate revert restored the stage")
	}
}

// onRollback is ungated by the per-revision action ledger, so it fires on EVERY
// rollback — two consecutive failing reconciles that each roll back the same
// pinned revision both run it. (A stage's ledger-gated onFailure would fire only
// once for that revision.)
func TestReconcile_OnRollback_UngatedByLedger_FiresEveryRollback(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	servedArtifact(t, c, ns, "ea-a", "", map[string]string{"cm.yaml": cmValManifest(ns, "shared", "v1")})
	servedArtifact(t, c, ns, "ea-b", "", map[string]string{"cm.yaml": configMapManifest(ns, "obj-b")})

	var hits int32
	url := countingServer(t, http.StatusOK, &hits)

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "rb-ledger"},
		Spec: stagesv1.StageSetSpec{
			Interval:          metav1.Duration{Duration: time.Minute},
			RollbackOnFailure: true,
			OnRollback: []stagesv1.Action{{
				Name: "notify",
				HTTP: &stagesv1.HTTPAction{URL: url, Method: http.MethodPost},
			}},
			Stages: []stagesv1.Stage{
				{Name: "stage-a", SourceRef: stagesv1.SourceReference{Name: "ea-a"}},
				{Name: "stage-b", SourceRef: stagesv1.SourceReference{Name: "ea-b"}},
			},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	allowed := []string{actionHost(t, url)}

	// Success first so a snapshot exists, then break stage-b so every following
	// reconcile fails and rolls back the same pinned revision.
	if _, err := driveReconcile(onRollbackReconciler(c, nil, allowed), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "rb-ledger"}}); err != nil {
		t.Fatalf("initial success: %v", err)
	}
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Fatalf("onRollback must not fire on the successful run, hits = %d", n)
	}
	repointArtifact(t, c, ns, "ea-b", map[string]string{"cm.yaml": cmValManifest(ns, "Bad_Name", "x")})

	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "rb-ledger"}}
	for pass := 1; pass <= 2; pass++ {
		// The failing reconcile rolls back (and runs onRollback) before returning
		// the stage failure, so the error is expected and discarded.
		_, _ = driveReconcile(onRollbackReconciler(c, nil, allowed), req)
	}
	if n := atomic.LoadInt32(&hits); n != 2 {
		t.Fatalf("onRollback should fire on every rollback (2 failing passes), hits = %d", n)
	}
}

// onRollback is best-effort: an action that fails emits a Warning event but does
// not change the rollback outcome — the manifests still restored, the run still
// reports RolledBack + StageFailed. Here the http action's loopback host is not
// in the (empty) allow-list, so it is forbidden and fails.
func TestReconcile_OnRollback_BestEffortFailureEmitsWarning(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	servedArtifact(t, c, ns, "ea-a", "", map[string]string{"cm.yaml": cmValManifest(ns, "shared", "v1")})
	servedArtifact(t, c, ns, "ea-b", "", map[string]string{"cm.yaml": configMapManifest(ns, "obj-b")})

	var hits int32
	url := countingServer(t, http.StatusOK, &hits)

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "rb-besteffort"},
		Spec: stagesv1.StageSetSpec{
			Interval:          metav1.Duration{Duration: time.Minute},
			RollbackOnFailure: true,
			OnRollback: []stagesv1.Action{{
				Name: "notify",
				HTTP: &stagesv1.HTTPAction{URL: url, Method: http.MethodPost},
			}},
			Stages: []stagesv1.Stage{
				{Name: "stage-a", SourceRef: stagesv1.SourceReference{Name: "ea-a"}},
				{Name: "stage-b", SourceRef: stagesv1.SourceReference{Name: "ea-b"}},
			},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	rec := &capturingRecorder{}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "rb-besteffort"}}

	if _, err := driveReconcile(onRollbackReconciler(c, rec, nil), req); err != nil {
		t.Fatalf("initial success: %v", err)
	}
	repointArtifact(t, c, ns, "ea-b", map[string]string{"cm.yaml": cmValManifest(ns, "Bad_Name", "x")})
	// The failing reconcile rolls back before returning the error; discard it.
	_, _ = driveReconcile(onRollbackReconciler(c, rec, nil), req)

	if got := readyReason(getStageSet(t, c, ns, "rb-besteffort")); got != ReasonStageFailed {
		t.Fatalf("run should report StageFailed, got %q", got)
	}
	if v := cmDataKey(t, c, ns, "shared"); v != "v1" {
		t.Fatalf("rollback should restore shared to v1 despite the failing onRollback action, got %q", v)
	}
	if n := rec.countEvents(eventReasonRolledBack, "rolled back"); n != 1 {
		t.Fatalf("rollback should still report RolledBack, got %d events", n)
	}
	if rec.countEvents("OnRollbackAction", "notify") == 0 {
		t.Fatal("a failing onRollback action should emit an OnRollbackAction Warning event")
	}
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Fatalf("the forbidden action must never reach the server, hits = %d", n)
	}
}

// Actions in the onRollback list run in list order.
func TestReconcile_OnRollback_ActionsRunInListOrder(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	servedArtifact(t, c, ns, "ea-a", "", map[string]string{"cm.yaml": cmValManifest(ns, "shared", "v1")})
	servedArtifact(t, c, ns, "ea-b", "", map[string]string{"cm.yaml": configMapManifest(ns, "obj-b")})

	var mu sync.Mutex
	var order []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		order = append(order, strings.TrimPrefix(r.URL.Path, "/"))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "rb-order"},
		Spec: stagesv1.StageSetSpec{
			Interval:          metav1.Duration{Duration: time.Minute},
			RollbackOnFailure: true,
			OnRollback: []stagesv1.Action{
				{Name: "first", HTTP: &stagesv1.HTTPAction{URL: srv.URL + "/first", Method: http.MethodPost}},
				{Name: "second", HTTP: &stagesv1.HTTPAction{URL: srv.URL + "/second", Method: http.MethodPost}},
			},
			Stages: []stagesv1.Stage{
				{Name: "stage-a", SourceRef: stagesv1.SourceReference{Name: "ea-a"}},
				{Name: "stage-b", SourceRef: stagesv1.SourceReference{Name: "ea-b"}},
			},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	allowed := []string{actionHost(t, srv.URL)}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "rb-order"}}

	if _, err := driveReconcile(onRollbackReconciler(c, nil, allowed), req); err != nil {
		t.Fatalf("initial success: %v", err)
	}
	repointArtifact(t, c, ns, "ea-b", map[string]string{"cm.yaml": cmValManifest(ns, "Bad_Name", "x")})
	// The failing reconcile rolls back before returning the error; discard it.
	_, _ = driveReconcile(onRollbackReconciler(c, nil, allowed), req)

	mu.Lock()
	defer mu.Unlock()
	if len(order) != 2 || order[0] != "first" || order[1] != "second" {
		t.Fatalf("onRollback actions ran out of order: %v", order)
	}
}
