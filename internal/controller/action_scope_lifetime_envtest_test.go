// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

// lifetimeStageSet builds a versioned StageSet with one scope: Lifetime post
// action hitting a counter.
func lifetimeStageSet(ns, name, endpoint string) *stagesv1.StageSet {
	return &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: stagesv1.StageSetSpec{
			Interval: metav1.Duration{Duration: time.Minute},
			Version:  &stagesv1.VersionSource{Value: "1.0.0"},
			Stages: []stagesv1.Stage{{
				Name:      "app",
				SourceRef: stagesv1.SourceReference{Name: "ea"},
				Actions: &stagesv1.StageActions{Post: []stagesv1.Action{
					{Name: "install-database", Scope: stagesv1.ScopeLifetime, HTTP: &stagesv1.HTTPAction{URL: endpoint}},
				}},
			}},
		},
	}
}

func getLedger(t *testing.T, c client.Client, ns, name string) *stagesv1.StageLedger {
	t.Helper()
	var l stagesv1.StageLedger
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &l); err != nil {
		t.Fatalf("get StageLedger %s/%s: %v", ns, name, err)
	}
	return &l
}

// The Lifetime guarantee: a scope: Lifetime action runs exactly once and never
// again — not on revision churn, not on a version change, not on a
// force-reconcile. Its completion lives in a StageLedger, unreachable by every
// in-status clearing path.
func TestActionScope_LifetimeRunsOnceEver(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	var hits int32
	endpoint := countingServer(t, http.StatusOK, &hits)
	hosts := []string{actionHost(t, endpoint)}

	servedArtifact(t, c, ns, "ea", "", map[string]string{"cm.yaml": configMapManifest(ns, "obj")})
	ss := lifetimeStageSet(ns, "boot", endpoint)
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}

	// Fresh install: the bootstrap runs once and is recorded Executed.
	if err := reconcileWith(t, c, ss, hosts); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("Lifetime action should run once on first install, ran %d", n)
	}
	ledger := getLedger(t, c, ns, "boot")
	if !ledger.IsCompleted("app", "install-database") {
		t.Fatalf("completion not recorded in the StageLedger: %+v", ledger.Status.CompletedActions)
	}
	if ledger.Status.CompletedActions[0].Origin != stagesv1.OriginExecuted {
		t.Errorf("origin = %q, want Executed", ledger.Status.CompletedActions[0].Origin)
	}

	// Revision churn.
	repointArtifact(t, c, ns, "ea", map[string]string{"cm.yaml": configMapManifest(ns, "obj-v2")})
	if err := reconcileWith(t, c, ss, hosts); err != nil {
		t.Fatalf("reconcile 2 (churn): %v", err)
	}
	// Version change.
	live := getStageSet(t, c, ns, "boot")
	live.Spec.Version.Value = "2.0.0"
	if err := c.Update(context.Background(), live); err != nil {
		t.Fatalf("bump version: %v", err)
	}
	if err := reconcileWith(t, c, live, hosts); err != nil {
		t.Fatalf("reconcile 3 (version change): %v", err)
	}
	// Force-reconcile the stage (clears the revision ledger, not the lifetime one).
	stampAnnotation(t, c, ns, "boot", "stages.metio.wtf/reconcile-stage", "app@tok-1")
	live = getStageSet(t, c, ns, "boot")
	if err := reconcileWith(t, c, live, hosts); err != nil {
		t.Fatalf("reconcile 4 (force): %v", err)
	}

	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Errorf("Lifetime action ran %d times; once ever means 1 across revision, version, and force changes", n)
	}
}

// spec.baseline suppresses a Lifetime action without running it — adoption of a
// system whose bootstrap already ran elsewhere. The ledger records it Baselined.
func TestActionScope_LifetimeBaselineSuppresses(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	var hits int32
	endpoint := countingServer(t, http.StatusOK, &hits)
	hosts := []string{actionHost(t, endpoint)}

	servedArtifact(t, c, ns, "ea", "", map[string]string{"cm.yaml": configMapManifest(ns, "obj")})
	ss := lifetimeStageSet(ns, "adopt", endpoint)
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	// Pre-declare the ledger with a baseline assertion (adoption).
	ledger := &stagesv1.StageLedger{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "adopt"},
		Spec:       stagesv1.StageLedgerSpec{Baseline: []stagesv1.LedgerRef{{Stage: "app", Action: "install-database"}}},
	}
	if err := c.Create(context.Background(), ledger); err != nil {
		t.Fatalf("create ledger: %v", err)
	}

	if err := reconcileWith(t, c, ss, hosts); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Errorf("baselined Lifetime action ran %d times; a baseline must suppress it (want 0)", n)
	}
	got := getLedger(t, c, ns, "adopt")
	if !got.IsCompleted("app", "install-database") {
		t.Fatal("baseline was not promoted into completedActions")
	}
	if got.Status.CompletedActions[0].Origin != stagesv1.OriginBaselined {
		t.Errorf("origin = %q, want Baselined", got.Status.CompletedActions[0].Origin)
	}
}

// The ledger is not owner-referenced, so deleting and recreating the StageSet
// does not forget the bootstrap: the recreated StageSet adopts the retained
// ledger and does not re-run install-database.
func TestActionScope_LifetimeSurvivesStageSetRecreate(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	var hits int32
	endpoint := countingServer(t, http.StatusOK, &hits)
	hosts := []string{actionHost(t, endpoint)}

	servedArtifact(t, c, ns, "ea", "", map[string]string{"cm.yaml": configMapManifest(ns, "obj")})
	ss := lifetimeStageSet(ns, "keep", endpoint)
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	if err := reconcileWith(t, c, ss, hosts); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("bootstrap should run once, ran %d", n)
	}

	// Delete the StageSet (drop the finalizer path is exercised elsewhere; here we
	// just remove it) and recreate the same-named one.
	live := getStageSet(t, c, ns, "keep")
	live.Finalizers = nil
	if err := c.Update(context.Background(), live); err != nil {
		t.Fatalf("clear finalizers: %v", err)
	}
	if err := c.Delete(context.Background(), live); err != nil {
		t.Fatalf("delete StageSet: %v", err)
	}
	ss2 := lifetimeStageSet(ns, "keep", endpoint)
	if err := c.Create(context.Background(), ss2); err != nil {
		t.Fatalf("recreate StageSet: %v", err)
	}
	if err := reconcileWith(t, c, ss2, hosts); err != nil {
		t.Fatalf("reconcile after recreate: %v", err)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Errorf("bootstrap re-ran after delete+recreate (ran %d); the retained ledger must suppress it", n)
	}
}
