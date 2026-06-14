// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"testing"
	"time"

	fluxmeta "github.com/fluxcd/pkg/apis/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

// A reconcile request (reconcile.fluxcd.io/requestedAt) is recorded in
// status.lastHandledReconcileAt once handled, so `flux reconcile` can detect
// completion.
func TestReconcile_RecordsHandledReconcileRequest(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	servedArtifact(t, c, ns, "art", "", map[string]string{"cm.yaml": configMapManifest(ns, "applied")})

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   ns,
			Name:        "triggered",
			Annotations: map[string]string{fluxmeta.ReconcileRequestAnnotation: "token-1"},
		},
		Spec: stagesv1.StageSetSpec{
			Interval: metav1.Duration{Duration: time.Minute},
			Stages:   []stagesv1.Stage{{Name: "stage-a", SourceRef: stagesv1.SourceReference{Name: "art"}}},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	reconcileOnce(t, c, ss)

	if got := getStageSet(t, c, ns, "triggered").Status.GetLastHandledReconcileRequest(); got != "token-1" {
		t.Fatalf("lastHandledReconcileAt = %q, want %q", got, "token-1")
	}

	// A new request token is handled on the next reconcile.
	cur := getStageSet(t, c, ns, "triggered")
	cur.Annotations[fluxmeta.ReconcileRequestAnnotation] = "token-2"
	if err := c.Update(context.Background(), cur); err != nil {
		t.Fatalf("update annotation: %v", err)
	}
	reconcileOnce(t, c, cur)
	if got := getStageSet(t, c, ns, "triggered").Status.GetLastHandledReconcileRequest(); got != "token-2" {
		t.Fatalf("after re-request, lastHandledReconcileAt = %q, want %q", got, "token-2")
	}
}

// A suspended StageSet does not act on the request, so it is not recorded as
// handled — `flux reconcile` correctly reports the resource as not progressing.
func TestReconcile_SuspendedDoesNotRecordReconcileRequest(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   ns,
			Name:        "paused",
			Annotations: map[string]string{fluxmeta.ReconcileRequestAnnotation: "token-1"},
		},
		Spec: stagesv1.StageSetSpec{
			Interval: metav1.Duration{Duration: time.Minute},
			Suspend:  true,
			Stages:   []stagesv1.Stage{{Name: "stage-a", SourceRef: stagesv1.SourceReference{Name: "art"}}},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	reconcileOnce(t, c, ss)

	if got := getStageSet(t, c, ns, "paused").Status.GetLastHandledReconcileRequest(); got != "" {
		t.Fatalf("suspended lastHandledReconcileAt = %q, want empty", got)
	}
}
