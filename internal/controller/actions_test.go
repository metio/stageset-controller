// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
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

func reconcileWith(t *testing.T, c client.Client, ss *stagesv1.StageSet, allowedHosts []string) error {
	t.Helper()
	r := &StageSetReconciler{
		Client:             c,
		RESTMapper:         c.RESTMapper(),
		Fetcher:            &artifact.Fetcher{HTTPClient: http.DefaultClient, URLValidator: artifact.PermissiveHTTPURL, IPValidator: artifact.PermissiveIP},
		AllowedActionHosts: allowedHosts,
		// httptest action listeners bind loopback; the production dial-time pin
		// would reject them, so opt the test path into a permissive validator.
		ActionIPValidator: actions.PermissiveIP,
	}
	_, err := driveReconcile(r, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ss.Namespace, Name: ss.Name}})
	return err
}

func actionHost(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u.Hostname()
}

func countingServer(t *testing.T, status int, counter *int32) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(counter, 1)
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// A pre-action patches an existing in-cluster object before the stage applies.
func TestReconcile_PreActionPatch(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)

	target := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "maintenance"},
		Data:       map[string]string{"state": "off"},
	}
	if err := c.Create(context.Background(), target); err != nil {
		t.Fatalf("create target: %v", err)
	}
	servedArtifact(t, c, ns, "ea", "", map[string]string{"cm.yaml": configMapManifest(ns, "stage-obj")})

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "with-pre"},
		Spec: stagesv1.StageSetSpec{
			Interval: metav1.Duration{Duration: time.Minute},
			Stages: []stagesv1.Stage{{
				Name:      "stage-a",
				SourceRef: stagesv1.SourceReference{Name: "ea"},
				Actions: &stagesv1.StageActions{Pre: []stagesv1.Action{{
					Name: "maintenance-on",
					Patch: &stagesv1.PatchAction{
						Target: stagesv1.PatchTarget{APIVersion: "v1", Kind: "ConfigMap", Name: "maintenance", Namespace: ns},
						Patch:  `{"data":{"state":"on"}}`,
					},
				}}},
			}},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	if err := reconcileWith(t, c, ss, nil); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var patched corev1.ConfigMap
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "maintenance"}, &patched); err != nil {
		t.Fatalf("get target: %v", err)
	}
	if patched.Data["state"] != "on" {
		t.Fatalf("pre-action patch not applied: %#v", patched.Data)
	}
	if !cmExists(t, c, ns, "stage-obj") {
		t.Fatal("the stage's own object should apply after the pre-action")
	}
	if readyReason(getStageSet(t, c, ns, "with-pre")) != ReasonReady {
		t.Fatal("stage with a successful pre-action should be Ready")
	}
}

// A delete action removes a named in-cluster object; a missing target is
// success (idempotent), so the action does not re-fail on later reconciles.
func TestReconcile_DeleteAction(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)

	victim := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "obsolete"}}
	if err := c.Create(context.Background(), victim); err != nil {
		t.Fatalf("create victim: %v", err)
	}
	servedArtifact(t, c, ns, "ea", "", map[string]string{"cm.yaml": configMapManifest(ns, "stage-obj")})

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "deleter"},
		Spec: stagesv1.StageSetSpec{
			Interval: metav1.Duration{Duration: time.Minute},
			Stages: []stagesv1.Stage{{
				Name:      "stage-a",
				SourceRef: stagesv1.SourceReference{Name: "ea"},
				Actions: &stagesv1.StageActions{Pre: []stagesv1.Action{{
					Name:   "drop-obsolete",
					Delete: &stagesv1.DeleteAction{Target: meta.NamespacedObjectKindReference{APIVersion: "v1", Kind: "ConfigMap", Name: "obsolete", Namespace: ns}},
				}}},
			}},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	if err := reconcileWith(t, c, ss, nil); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var gone corev1.ConfigMap
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "obsolete"}, &gone); !apierrors.IsNotFound(err) {
		t.Fatalf("delete action should have removed the object, get err = %v", err)
	}
	if !cmExists(t, c, ns, "stage-obj") || readyReason(getStageSet(t, c, ns, "deleter")) != ReasonReady {
		t.Fatal("the stage should apply and be Ready after the delete action")
	}

	// A second reconcile: the target is already gone, so the action is a no-op
	// (idempotent), not a failure.
	if err := reconcileWith(t, c, ss, nil); err != nil {
		t.Fatalf("second reconcile (target already deleted) should succeed: %v", err)
	}
}

// An apply action stands up manifests from an artifact that are NOT recorded in
// the stage inventory (so the inventory diff never prunes them) — the basis for
// transient, rollout-scoped resources.
func TestReconcile_ApplyAction_AppliesTransientObject(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	servedArtifact(t, c, ns, "ea", "", map[string]string{"cm.yaml": configMapManifest(ns, "stage-obj")})
	servedArtifact(t, c, ns, "maint", "", map[string]string{"m.yaml": configMapManifest(ns, "maintenance-page")})

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "with-apply"},
		Spec: stagesv1.StageSetSpec{
			Interval: metav1.Duration{Duration: time.Minute},
			Stages: []stagesv1.Stage{{
				Name:      "stage-a",
				SourceRef: stagesv1.SourceReference{Name: "ea"},
				Actions: &stagesv1.StageActions{Pre: []stagesv1.Action{{
					Name:  "maint-up",
					Apply: &stagesv1.ApplyAction{SourceRef: stagesv1.SourceReference{Name: "maint"}},
				}}},
			}},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	if err := reconcileWith(t, c, ss, nil); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if !cmExists(t, c, ns, "maintenance-page") {
		t.Fatal("apply action should have created the transient object")
	}
	if !cmExists(t, c, ns, "stage-obj") {
		t.Fatal("the stage's own object should still apply")
	}
	// The apply-action object must not be inventory-tracked: the stage owns
	// only its own object.
	if n := inventoryEntryCount(t, c, ns, "with-apply", "stage-a"); n != 1 {
		t.Fatalf("apply-action objects must not be in the stage inventory; got %d entries", n)
	}
	if readyReason(getStageSet(t, c, ns, "with-apply")) != ReasonReady {
		t.Fatal("a stage with a successful apply action should be Ready")
	}
}

// The maintenance-page pattern: an apply action stands a resource up in a
// stage's pre-step and a delete action tears it down in the post-step, so the
// transient object does not survive the rollout.
func TestReconcile_ApplyThenDelete_TransientLifecycle(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	servedArtifact(t, c, ns, "ea", "", map[string]string{"cm.yaml": configMapManifest(ns, "stage-obj")})
	servedArtifact(t, c, ns, "maint", "", map[string]string{"m.yaml": configMapManifest(ns, "maintenance-page")})

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "maint-cycle"},
		Spec: stagesv1.StageSetSpec{
			Interval: metav1.Duration{Duration: time.Minute},
			Stages: []stagesv1.Stage{{
				Name:      "stage-a",
				SourceRef: stagesv1.SourceReference{Name: "ea"},
				Actions: &stagesv1.StageActions{
					Pre: []stagesv1.Action{{
						Name:  "maint-up",
						Apply: &stagesv1.ApplyAction{SourceRef: stagesv1.SourceReference{Name: "maint"}},
					}},
					Post: []stagesv1.Action{{
						Name:   "maint-down",
						Delete: &stagesv1.DeleteAction{Target: meta.NamespacedObjectKindReference{APIVersion: "v1", Kind: "ConfigMap", Name: "maintenance-page", Namespace: ns}},
					}},
				},
			}},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	if err := reconcileWith(t, c, ss, nil); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if cmExists(t, c, ns, "maintenance-page") {
		t.Fatal("the transient object should be torn down by the delete action")
	}
	if !cmExists(t, c, ns, "stage-obj") || readyReason(getStageSet(t, c, ns, "maint-cycle")) != ReasonReady {
		t.Fatal("the rollout should complete Ready after the transient lifecycle")
	}
}

// A post (http) action fires exactly once across reconciles of the same pinned
// revision, proving the idempotency ledger.
func TestReconcile_ActionLedgerIdempotent(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	var hits int32
	endpoint := countingServer(t, http.StatusOK, &hits)

	servedArtifact(t, c, ns, "ea", "", map[string]string{"cm.yaml": configMapManifest(ns, "obj")})
	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "ledger"},
		Spec: stagesv1.StageSetSpec{
			Interval: metav1.Duration{Duration: time.Minute},
			Stages: []stagesv1.Stage{{
				Name:      "stage-a",
				SourceRef: stagesv1.SourceReference{Name: "ea"},
				Actions:   &stagesv1.StageActions{Post: []stagesv1.Action{{Name: "notify", HTTP: &stagesv1.HTTPAction{URL: endpoint}}}},
			}},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	hosts := []string{actionHost(t, endpoint)}
	if err := reconcileWith(t, c, ss, hosts); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	if err := reconcileWith(t, c, ss, hosts); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("post action should fire once across reconciles, fired %d times", n)
	}
}

// A wait action with a CEL expression that the target already satisfies
// returns immediately and the stage proceeds.
func TestReconcile_WaitExprAction(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)

	target := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "drainable", Labels: map[string]string{"ready": "yes"}},
	}
	if err := c.Create(context.Background(), target); err != nil {
		t.Fatalf("create target: %v", err)
	}
	servedArtifact(t, c, ns, "ea", "", map[string]string{"cm.yaml": configMapManifest(ns, "obj")})

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "waiter"},
		Spec: stagesv1.StageSetSpec{
			Interval: metav1.Duration{Duration: time.Minute},
			Stages: []stagesv1.Stage{{
				Name:      "stage-a",
				SourceRef: stagesv1.SourceReference{Name: "ea"},
				Actions: &stagesv1.StageActions{Pre: []stagesv1.Action{{
					Name: "drain",
					Wait: &stagesv1.WaitAction{
						Target:  &meta.NamespacedObjectKindReference{APIVersion: "v1", Kind: "ConfigMap", Name: "drainable", Namespace: ns},
						Expr:    `metadata.labels.ready == 'yes'`,
						Timeout: &metav1.Duration{Duration: 30 * time.Second},
					},
				}}},
			}},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	if err := reconcileWith(t, c, ss, nil); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if readyReason(getStageSet(t, c, ns, "waiter")) != ReasonReady {
		t.Fatal("a satisfied wait-expr should let the stage become Ready")
	}
}

// A failing post action runs the stage's onFailure actions and fails the stage.
func TestReconcile_OnFailureRuns(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	var failHits, notifyHits int32
	failURL := countingServer(t, http.StatusInternalServerError, &failHits)
	notifyURL := countingServer(t, http.StatusOK, &notifyHits)

	servedArtifact(t, c, ns, "ea", "", map[string]string{"cm.yaml": configMapManifest(ns, "obj")})
	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "onfail"},
		Spec: stagesv1.StageSetSpec{
			Interval: metav1.Duration{Duration: time.Minute},
			Stages: []stagesv1.Stage{{
				Name:      "stage-a",
				SourceRef: stagesv1.SourceReference{Name: "ea"},
				Actions: &stagesv1.StageActions{
					Post:      []stagesv1.Action{{Name: "smoke", HTTP: &stagesv1.HTTPAction{URL: failURL}}},
					OnFailure: []stagesv1.Action{{Name: "page", HTTP: &stagesv1.HTTPAction{URL: notifyURL}}},
				},
			}},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	hosts := []string{actionHost(t, failURL), actionHost(t, notifyURL)}
	if err := reconcileWith(t, c, ss, hosts); err == nil {
		t.Fatal("a failing post action should fail the reconcile")
	}
	if atomic.LoadInt32(&notifyHits) != 1 {
		t.Fatalf("onFailure action should fire once, fired %d times", notifyHits)
	}
	if readyReason(getStageSet(t, c, ns, "onfail")) != ReasonStageFailed {
		t.Fatal("a failed post action should leave the stage Failed")
	}
}
