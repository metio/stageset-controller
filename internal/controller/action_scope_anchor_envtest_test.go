// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

// anchoredStageSet is lifetimeStageSet with a completionAnchor on the
// install-database action pointing at the ConfigMap the stage applies.
func anchoredStageSet(ns, name, endpoint string) *stagesv1.StageSet {
	ss := lifetimeStageSet(ns, name, endpoint)
	ss.Spec.Stages[0].Actions.Post[0].CompletionAnchor = &stagesv1.CompletionAnchor{
		APIVersion: "v1", Kind: "ConfigMap", Name: "obj",
	}
	return ss
}

func getConfigMap(t *testing.T, c client.Client, ns, name string) *corev1.ConfigMap {
	t.Helper()
	var cm corev1.ConfigMap
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &cm); err != nil {
		t.Fatalf("get ConfigMap %s/%s: %v", ns, name, err)
	}
	return &cm
}

func findCompletion(l *stagesv1.StageLedger, stage, action string) *stagesv1.LedgerCompletion {
	for i := range l.Status.CompletedActions {
		c := &l.Status.CompletedActions[i]
		if c.Stage == stage && c.Action == action {
			return c
		}
	}
	return nil
}

// An anchored scope: Lifetime action records the witness object's UID at
// completion, and a steady reconcile with the witness unchanged does not re-run
// it — the anchor gates exactly like an unanchored completion while the witness
// is stable.
func TestActionScope_AnchoredRecordsWitnessUID(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	var hits int32
	endpoint := countingServer(t, http.StatusOK, &hits)
	hosts := []string{actionHost(t, endpoint)}

	servedArtifact(t, c, ns, "ea", "", map[string]string{"cm.yaml": configMapManifest(ns, "obj")})
	ss := anchoredStageSet(ns, "boot", endpoint)
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	if err := reconcileWith(t, c, ss, hosts); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("anchored action should run once, ran %d", n)
	}

	ledger := getLedger(t, c, ns, "boot")
	comp := findCompletion(ledger, "app", "install-database")
	if comp == nil || comp.Anchor == nil {
		t.Fatalf("completion must carry an anchor witness: %+v", ledger.Status.CompletedActions)
	}
	cm := getConfigMap(t, c, ns, "obj")
	if comp.Anchor.UID != string(cm.UID) {
		t.Errorf("recorded anchor UID = %q, want the applied ConfigMap's UID %q", comp.Anchor.UID, cm.UID)
	}

	// Steady reconcile with the witness unchanged: no re-run.
	if err := reconcileWith(t, c, ss, hosts); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Errorf("anchored action re-ran with a stable witness (ran %d); once ever means 1", n)
	}
}

// When the witness is gone — here the ConfigMap is deleted and re-applied by the
// stage under a fresh UID — the completion invalidates, a LedgerInvalidated
// event fires, and the bootstrap runs again, re-recording the new witness UID.
// This is the self-composition case: destroying the state the bootstrap
// initialized re-runs the bootstrap.
func TestActionScope_AnchorInvalidationReRuns(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	var hits int32
	endpoint := countingServer(t, http.StatusOK, &hits)
	hosts := []string{actionHost(t, endpoint)}

	servedArtifact(t, c, ns, "ea", "", map[string]string{"cm.yaml": configMapManifest(ns, "obj")})
	ss := anchoredStageSet(ns, "reboot", endpoint)
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	if err := reconcileWith(t, c, ss, hosts); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	firstUID := findCompletion(getLedger(t, c, ns, "reboot"), "app", "install-database").Anchor.UID

	// Destroy the witness. The gate sees it absent; the stage re-applies it (fresh
	// UID) before the post action re-records.
	cm := getConfigMap(t, c, ns, "obj")
	if err := c.Delete(context.Background(), cm); err != nil {
		t.Fatalf("delete witness: %v", err)
	}
	rec := &capturingRecorder{}
	if err := reconcileWithRecorder(t, c, ss, hosts, rec); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}

	if n := atomic.LoadInt32(&hits); n != 2 {
		t.Fatalf("bootstrap must re-run when its witness is gone; ran %d, want 2", n)
	}
	if !rec.has(eventReasonLedgerInvalidated) {
		t.Errorf("expected a %q event on anchor invalidation", eventReasonLedgerInvalidated)
	}
	comp := findCompletion(getLedger(t, c, ns, "reboot"), "app", "install-database")
	if comp == nil || comp.Anchor == nil {
		t.Fatal("re-run must re-record the completion with a fresh witness")
	}
	newCM := getConfigMap(t, c, ns, "obj")
	if comp.Anchor.UID != string(newCM.UID) {
		t.Errorf("re-recorded anchor UID = %q, want the recreated ConfigMap's UID %q", comp.Anchor.UID, newCM.UID)
	}
	if comp.Anchor.UID == firstUID {
		t.Error("anchor UID was not refreshed after invalidation")
	}
}

// An anchored action whose witness does not exist at completion fails the action
// (it can never record a UID, so an unrecorded completion would re-run forever
// silently). The stage goes non-Ready and nothing is recorded.
func TestActionScope_AnchorAbsentAtCompletionFails(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	var hits int32
	endpoint := countingServer(t, http.StatusOK, &hits)
	hosts := []string{actionHost(t, endpoint)}

	servedArtifact(t, c, ns, "ea", "", map[string]string{"cm.yaml": configMapManifest(ns, "obj")})
	ss := lifetimeStageSet(ns, "ghost", endpoint)
	// Anchor a witness the stage never applies.
	ss.Spec.Stages[0].Actions.Post[0].CompletionAnchor = &stagesv1.CompletionAnchor{
		APIVersion: "v1", Kind: "ConfigMap", Name: "never-applied",
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	_ = reconcileWith(t, c, ss, hosts) // the stage fails; the reconcile result is recorded in status

	got := getStageSet(t, c, ns, "ghost")
	if cond := apimeta.FindStatusCondition(got.Status.Conditions, ConditionReady); cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("stage must be non-Ready when the anchor is absent at completion; Ready = %+v", cond)
	}
	if getLedger(t, c, ns, "ghost").IsCompleted("app", "install-database") {
		t.Error("no completion may be recorded when the anchor cannot be witnessed")
	}
}
