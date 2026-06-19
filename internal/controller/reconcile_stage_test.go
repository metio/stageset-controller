// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	fluxmeta "github.com/fluxcd/pkg/apis/meta"
	fluxpredicates "github.com/fluxcd/pkg/runtime/predicates"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"sigs.k8s.io/controller-runtime/pkg/client"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

// The watch predicate is predicate.Or(GenerationChanged, ReconcileRequested,
// reconcileStageRequested). The single-stage reconcile-stage annotation bumps
// neither generation nor the Flux requestedAt token, so without the third term
// the force-stage Update event would be dropped — this pins that it survives,
// and that status-only / unrelated-annotation churn does not.
func TestWatchPredicate_WakesOnExpectedChanges(t *testing.T) {
	t.Parallel()
	pred := predicate.Or(
		predicate.GenerationChangedPredicate{},
		fluxpredicates.ReconcileRequestedPredicate{},
		reconcileStageRequestedPredicate{},
	)

	base := func() *stagesv1.StageSet {
		return &stagesv1.StageSet{ObjectMeta: metav1.ObjectMeta{
			Name: "ss", Namespace: "ns", Generation: 1,
		}}
	}
	withGen := func(g int64) *stagesv1.StageSet { s := base(); s.Generation = g; return s }
	withAnn := func(k, v string) *stagesv1.StageSet {
		s := base()
		s.Annotations = map[string]string{k: v}
		return s
	}

	cases := map[string]struct {
		oldObj, newObj *stagesv1.StageSet
		want           bool
	}{
		"generation bump (spec change)":   {base(), withGen(2), true},
		"reconcile-stage annotation set":  {base(), withAnn(reconcileStageAnnotation, "stage-a@t1"), true},
		"reconcile-stage token changed":   {withAnn(reconcileStageAnnotation, "stage-a@t1"), withAnn(reconcileStageAnnotation, "stage-a@t2"), true},
		"flux requestedAt token set":      {base(), withAnn(fluxmeta.ReconcileRequestAnnotation, "now"), true},
		"no change (status-only update)":  {base(), base(), false},
		"unrelated annotation only":       {base(), withAnn("example.com/note", "hi"), false},
		"reconcile-stage token unchanged": {withAnn(reconcileStageAnnotation, "stage-a@t1"), withAnn(reconcileStageAnnotation, "stage-a@t1"), false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := pred.Update(event.UpdateEvent{ObjectOld: tc.oldObj, ObjectNew: tc.newObj})
			if got != tc.want {
				t.Errorf("predicate Update = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestParseReconcileStage(t *testing.T) {
	cases := map[string]struct {
		annotation string
		wantStage  string
		wantToken  string
	}{
		"well-formed":   {"canary@123", "canary", "123"},
		"token-with-at": {"canary@a@b", "canary", "a@b"},
		"missing-token": {"canary@", "", ""},
		"missing-stage": {"@123", "", ""},
		"no-separator":  {"canary", "", ""},
		"empty":         {"", "", ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ss := &stagesv1.StageSet{}
			if tc.annotation != "" {
				ss.Annotations = map[string]string{reconcileStageAnnotation: tc.annotation}
			}
			stage, token := parseReconcileStage(ss)
			if stage != tc.wantStage || token != tc.wantToken {
				t.Errorf("parseReconcileStage(%q) = %q,%q want %q,%q", tc.annotation, stage, token, tc.wantStage, tc.wantToken)
			}
		})
	}
}

// A single-stage force-reconcile re-runs a stage's action exactly once per new
// token, even though the pinned revision is unchanged — and records the token
// so the same token does not fire twice.
func TestReconcile_ForceStageRerunsActions(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	var hits int32
	endpoint := countingServer(t, http.StatusOK, &hits)
	hosts := []string{actionHost(t, endpoint)}

	servedArtifact(t, c, ns, "ea", "", map[string]string{"cm.yaml": configMapManifest(ns, "obj")})
	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "forcer"},
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

	// Steady state: the action fires once and the ledger skips it thereafter.
	mustReconcile(t, c, ss, hosts)
	mustReconcile(t, c, ss, hosts)
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("action should fire once at steady state, fired %d", n)
	}

	// Force the stage: the action re-runs exactly once and the token is recorded.
	annotateStage(t, c, ns, "forcer", "stage-a@tok1")
	mustReconcile(t, c, ss, hosts)
	if n := atomic.LoadInt32(&hits); n != 2 {
		t.Fatalf("force should re-run the action, hits=%d want 2", n)
	}
	if got := stageToken(t, c, ns, "forcer", "stage-a"); got != "tok1" {
		t.Fatalf("lastHandledReconcileAt = %q want tok1", got)
	}

	// The same token does not fire again.
	mustReconcile(t, c, ss, hosts)
	if n := atomic.LoadInt32(&hits); n != 2 {
		t.Fatalf("same token should not re-fire, hits=%d want 2", n)
	}

	// A new token re-runs once more.
	annotateStage(t, c, ns, "forcer", "stage-a@tok2")
	mustReconcile(t, c, ss, hosts)
	if n := atomic.LoadInt32(&hits); n != 3 {
		t.Fatalf("new token should re-run, hits=%d want 3", n)
	}
	if got := stageToken(t, c, ns, "forcer", "stage-a"); got != "tok2" {
		t.Fatalf("lastHandledReconcileAt = %q want tok2", got)
	}
}

func mustReconcile(t *testing.T, c client.Client, ss *stagesv1.StageSet, hosts []string) {
	t.Helper()
	if err := reconcileWith(t, c, ss, hosts); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

// annotateStage stamps the reconcile-stage annotation on a StageSet, re-getting
// it first so the write carries the current resourceVersion.
func annotateStage(t *testing.T, c client.Client, ns, name, value string) {
	t.Helper()
	ss := getStageSet(t, c, ns, name)
	ann := ss.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	ann[reconcileStageAnnotation] = value
	ss.SetAnnotations(ann)
	if err := c.Update(context.Background(), ss); err != nil {
		t.Fatalf("annotate stage: %v", err)
	}
}

// stageToken reads a stage's recorded lastHandledReconcileAt.
func stageToken(t *testing.T, c client.Client, ns, name, stage string) string {
	t.Helper()
	ss := getStageSet(t, c, ns, name)
	for _, st := range ss.Status.Stages {
		if st.Name == stage {
			return st.LastHandledReconcileAt
		}
	}
	t.Fatalf("stage %q not found in status", stage)
	return ""
}
