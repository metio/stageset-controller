// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/actions"
	"github.com/metio/stageset-controller/internal/artifact"
)

// reconcileWithRecorder drives a reconcile like reconcileWith but wires a
// capturing event recorder so a test can assert emitted events.
func reconcileWithRecorder(t *testing.T, c client.Client, ss *stagesv1.StageSet, hosts []string, rec *capturingRecorder) error {
	t.Helper()
	r := &StageSetReconciler{
		Client:             c,
		RESTMapper:         c.RESTMapper(),
		Fetcher:            &artifact.Fetcher{HTTPClient: http.DefaultClient, URLValidator: artifact.PermissiveHTTPURL, IPValidator: artifact.PermissiveIP},
		AllowedActionHosts: hosts,
		ActionIPValidator:  actions.PermissiveIP,
		Recorder:           rec,
	}
	_, err := driveReconcile(r, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ss.Namespace, Name: ss.Name}})
	return err
}

// namedLifetimeStageSet is lifetimeStageSet with a caller-chosen action name.
func namedLifetimeStageSet(ns, name, action, endpoint string) *stagesv1.StageSet {
	return &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: stagesv1.StageSetSpec{
			Interval: metav1.Duration{Duration: time.Minute},
			Version:  &stagesv1.VersionSource{Value: "1.0.0"},
			Stages: []stagesv1.Stage{{
				Name:      "app",
				SourceRef: stagesv1.SourceReference{Name: "ea"},
				Actions: &stagesv1.StageActions{Post: []stagesv1.Action{
					{Name: action, Scope: stagesv1.ScopeLifetime, HTTP: &stagesv1.HTTPAction{URL: endpoint}},
				}},
			}},
		},
	}
}

func baselineCondition(l *stagesv1.StageLedger) *metav1.Condition {
	return apimeta.FindStatusCondition(l.Status.Conditions, stagesv1.LedgerConditionBaselineValid)
}

// A spec.baseline entry that resolves to no scope: Lifetime action must not be
// silently dropped: it is held (never promoted), the ledger's BaselineValid
// condition flips False, and a Warning event fires. A swallowed typo would let
// an operator believe they baselined an action they did not.
func TestActionScope_BaselineUnresolvable_HeldAndSurfaced(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	var hits int32
	endpoint := countingServer(t, http.StatusOK, &hits)
	hosts := []string{actionHost(t, endpoint)}
	rec := &capturingRecorder{}

	servedArtifact(t, c, ns, "ea", "", map[string]string{"cm.yaml": configMapManifest(ns, "obj")})
	ss := lifetimeStageSet(ns, "typo", endpoint) // real action: install-database
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	// Baseline names an action that does not exist in the spec.
	ledger := &stagesv1.StageLedger{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "typo"},
		Spec:       stagesv1.StageLedgerSpec{Baseline: []stagesv1.LedgerRef{{Stage: "app", Action: "instal-databse"}}},
	}
	if err := c.Create(context.Background(), ledger); err != nil {
		t.Fatalf("create ledger: %v", err)
	}

	if err := reconcileWithRecorder(t, c, ss, hosts, rec); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := getLedger(t, c, ns, "typo")
	if got.IsCompleted("app", "instal-databse") {
		t.Error("an unresolvable baseline entry must not be promoted into completedActions")
	}
	cond := baselineCondition(got)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "Unresolved" {
		t.Fatalf("BaselineValid condition = %+v; want Status=False Reason=Unresolved", cond)
	}
	if !rec.has(eventBaselineInvalid) {
		t.Errorf("expected a %q Warning event for the unresolvable baseline entry", eventBaselineInvalid)
	}
}

// A held baseline entry promotes automatically the moment a spec change makes it
// resolvable — "add the action and its baseline together" is a supported
// workflow, not an ordering hazard — and the BaselineValid condition clears. The
// promotion suppresses the action without running it (origin Baselined).
func TestActionScope_BaselineAutoPromotesWhenResolvable(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	var hits int32
	endpoint := countingServer(t, http.StatusOK, &hits)
	hosts := []string{actionHost(t, endpoint)}
	rec := &capturingRecorder{}

	servedArtifact(t, c, ns, "ea", "", map[string]string{"cm.yaml": configMapManifest(ns, "obj")})
	// The spec's only Lifetime action is "late-boot"; the baseline targets
	// "install-database", absent for now.
	ss := namedLifetimeStageSet(ns, "heal", "late-boot", endpoint)
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	ledger := &stagesv1.StageLedger{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "heal"},
		Spec:       stagesv1.StageLedgerSpec{Baseline: []stagesv1.LedgerRef{{Stage: "app", Action: "install-database"}}},
	}
	if err := c.Create(context.Background(), ledger); err != nil {
		t.Fatalf("create ledger: %v", err)
	}

	// Reconcile 1: install-database is unresolvable; late-boot runs (Executed).
	if err := reconcileWithRecorder(t, c, ss, hosts, rec); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	got := getLedger(t, c, ns, "heal")
	if got.IsCompleted("app", "install-database") {
		t.Fatal("install-database must stay held while absent from the spec")
	}
	if cond := baselineCondition(got); cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("BaselineValid = %+v; want False while unresolved", cond)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("late-boot should have run once, ran %d", n)
	}

	// Rename late-boot -> install-database: the baseline entry now resolves.
	live := getStageSet(t, c, ns, "heal")
	live.Spec.Stages[0].Actions.Post[0].Name = "install-database"
	if err := c.Update(context.Background(), live); err != nil {
		t.Fatalf("rename action: %v", err)
	}
	if err := reconcileWithRecorder(t, c, live, hosts, rec); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}

	got = getLedger(t, c, ns, "heal")
	if !got.IsCompleted("app", "install-database") {
		t.Fatal("install-database must auto-promote once the spec makes it resolvable")
	}
	if o := completionOrigin(got, "app", "install-database"); o != stagesv1.OriginBaselined {
		t.Errorf("auto-promoted entry origin = %q, want Baselined", o)
	}
	if cond := baselineCondition(got); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("BaselineValid = %+v; want True after resolution", cond)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Errorf("install-database was baselined and must never run; total hits = %d, want 1 (late-boot only)", n)
	}
}

// completionOrigin returns the recorded origin for (stage, action), or "".
func completionOrigin(l *stagesv1.StageLedger, stage, action string) stagesv1.LedgerOrigin {
	for i := range l.Status.CompletedActions {
		c := &l.Status.CompletedActions[i]
		if c.Stage == stage && c.Action == action {
			return c.Origin
		}
	}
	return ""
}
