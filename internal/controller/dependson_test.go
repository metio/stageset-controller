// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/fluxcd/pkg/apis/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/artifact"
)

// TestDependenciesReady_HeldNewRevisionGatesDependents pins that a dependency
// which is Deployed-and-Ready but holding a NEW revision behind an update window
// gates its dependents — otherwise the dependent rolls out against the
// dependency's old, about-to-be-replaced state. A pure freeze (no new revision
// pending) must still let dependents proceed.
func TestDependenciesReady_HeldNewRevisionGatesDependents(t *testing.T) {
	const ns = "team"
	readyDep := func(name string, pending *stagesv1.PendingUpdate) *stagesv1.StageSet {
		return &stagesv1.StageSet{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Generation: 1},
			Status: stagesv1.StageSetStatus{
				ObservedGeneration: 1,
				PendingUpdate:      pending,
				Conditions: []metav1.Condition{{
					Type:               ConditionReady,
					Status:             metav1.ConditionTrue,
					Reason:             ReasonReady,
					ObservedGeneration: 1,
					LastTransitionTime: metav1.Now(),
				}},
			},
		}
	}
	dependent := func(dep string) *stagesv1.StageSet {
		return &stagesv1.StageSet{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "dependent", Generation: 1},
			Spec:       stagesv1.StageSetSpec{DependsOn: []meta.NamespacedObjectReference{{Name: dep}}},
		}
	}
	newReconciler := func(dep *stagesv1.StageSet) *StageSetReconciler {
		c := fake.NewClientBuilder().WithScheme(watchScheme(t)).WithObjects(dep).Build()
		return &StageSetReconciler{Client: c}
	}

	t.Run("deployed dependency with no pending revision is ready", func(t *testing.T) {
		r := newReconciler(readyDep("dep", nil))
		ok, msg, err := r.dependenciesReady(context.Background(), dependent("dep"))
		if err != nil || !ok {
			t.Fatalf("dependenciesReady = (%v, %q, %v), want ready", ok, msg, err)
		}
	})

	t.Run("dependency holding a new revision gates the dependent", func(t *testing.T) {
		r := newReconciler(readyDep("dep", &stagesv1.PendingUpdate{Revisions: map[string]string{"ea": "sha256:new"}}))
		ok, msg, err := r.dependenciesReady(context.Background(), dependent("dep"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ok {
			t.Fatal("dependent must be gated while its dependency holds a new revision behind a window")
		}
		if !strings.Contains(msg, "held by an update window") {
			t.Errorf("gate message = %q, want it to mention the held update window", msg)
		}
	})

	t.Run("a pure freeze with no pending revision does not gate", func(t *testing.T) {
		r := newReconciler(readyDep("dep", &stagesv1.PendingUpdate{}))
		ok, _, err := r.dependenciesReady(context.Background(), dependent("dep"))
		if err != nil || !ok {
			t.Fatalf("a pure freeze (no new revision) must not gate dependents; got ok=%v err=%v", ok, err)
		}
	})
}

func stageSetDependsOn(t *testing.T, c client.Client, ns, name, eaName string, deps ...string) *stagesv1.StageSet {
	t.Helper()
	depRefs := make([]meta.NamespacedObjectReference, 0, len(deps))
	for _, d := range deps {
		depRefs = append(depRefs, meta.NamespacedObjectReference{Name: d})
	}
	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: stagesv1.StageSetSpec{
			Interval:  metav1.Duration{Duration: time.Minute},
			DependsOn: depRefs,
			Stages:    []stagesv1.Stage{{Name: "stage-a", SourceRef: stagesv1.SourceReference{Name: eaName}}},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	return ss
}

func TestReconcile_DependencyNotReady(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	servedArtifact(t, c, ns, "ea", "", map[string]string{"cm.yaml": configMapManifest(ns, "gated")})

	ss := stageSetDependsOn(t, c, ns, "dependent", "ea", "missing-dep")
	reconcileOnce(t, c, ss)

	if r := readyReason(getStageSet(t, c, ns, "dependent")); r != ReasonDependencyNotReady {
		t.Fatalf("Ready reason = %q, want %q", r, ReasonDependencyNotReady)
	}
	if cmExists(t, c, ns, "gated") {
		t.Fatal("a gated StageSet must not apply its stages")
	}
}

func TestReconcile_DependencyReady(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)

	servedArtifact(t, c, ns, "ea-a", "", map[string]string{"a.yaml": configMapManifest(ns, "dep-obj")})
	a := newStageSet(t, c, ns, "dep-a", stagesv1.SourceReference{Name: "ea-a"})
	reconcileOnce(t, c, a)
	if readyReason(getStageSet(t, c, ns, "dep-a")) != ReasonReady {
		t.Fatal("the dependency should be Ready after a successful run")
	}

	servedArtifact(t, c, ns, "ea-b", "", map[string]string{"b.yaml": configMapManifest(ns, "dependent-obj")})
	b := stageSetDependsOn(t, c, ns, "dep-b", "ea-b", "dep-a")
	reconcileOnce(t, c, b)

	if r := readyReason(getStageSet(t, c, ns, "dep-b")); r != ReasonReady {
		t.Fatalf("dependent should proceed once its dependency is Ready, reason = %q", r)
	}
	if !cmExists(t, c, ns, "dependent-obj") {
		t.Fatal("dependent should apply once the gate clears")
	}
}

func TestReconcile_DependencyCycle(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	servedArtifact(t, c, ns, "ea-x", "", map[string]string{"x.yaml": configMapManifest(ns, "cyc-x")})
	servedArtifact(t, c, ns, "ea-y", "", map[string]string{"y.yaml": configMapManifest(ns, "cyc-y")})

	stageSetDependsOn(t, c, ns, "cyc-x", "ea-x", "cyc-y")
	y := stageSetDependsOn(t, c, ns, "cyc-y", "ea-y", "cyc-x")
	reconcileOnce(t, c, y)

	if r := readyReason(getStageSet(t, c, ns, "cyc-y")); r != ReasonStalled {
		t.Fatalf("a dependsOn cycle should Stall, reason = %q", r)
	}
}

// A StageSet stuck on a terminal Ready=False reason (here a dependsOn cycle, a
// representative permanent failure) requeues on the bounded
// permanentRetryInterval rather than no-requeue, so an out-of-band fix —
// breaking the cycle, granting RBAC for an RBACDenied stage — self-heals within
// the interval without a watch event. The reconcile returns no error, so this
// is not controller-runtime's error backoff.
func TestReconcile_TerminalReasonRequeuesForRecovery(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	servedArtifact(t, c, ns, "ea-x", "", map[string]string{"x.yaml": configMapManifest(ns, "cyc-x")})
	servedArtifact(t, c, ns, "ea-y", "", map[string]string{"y.yaml": configMapManifest(ns, "cyc-y")})

	stageSetDependsOn(t, c, ns, "cyc-x", "ea-x", "cyc-y")
	y := stageSetDependsOn(t, c, ns, "cyc-y", "ea-y", "cyc-x")

	r := &StageSetReconciler{
		Client:     c,
		RESTMapper: c.RESTMapper(),
		Fetcher:    &artifact.Fetcher{HTTPClient: http.DefaultClient, URLValidator: artifact.PermissiveHTTPURL, IPValidator: artifact.PermissiveIP},
	}
	res, err := driveReconcile(r, ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: ns, Name: y.Name},
	})
	if err != nil {
		t.Fatalf("a terminal reason must not return an error (no backoff): %v", err)
	}
	if readyReason(getStageSet(t, c, ns, "cyc-y")) != ReasonStalled {
		t.Fatalf("precondition: a dependsOn cycle should Stall")
	}
	if res.RequeueAfter <= 0 {
		t.Fatalf("terminal reason must requeue on a bounded interval, got RequeueAfter = %v", res.RequeueAfter)
	}
	if res.RequeueAfter != permanentRetryInterval {
		t.Fatalf("RequeueAfter = %v, want permanentRetryInterval %v", res.RequeueAfter, permanentRetryInterval)
	}
}
