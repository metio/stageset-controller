// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"net/http"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/artifact"
	"github.com/metio/stageset-controller/internal/window"
)

func reconcileAt(t *testing.T, c client.Client, ss *stagesv1.StageSet, now time.Time) {
	t.Helper()
	r := &StageSetReconciler{
		Client:     c,
		RESTMapper: c.RESTMapper(),
		Fetcher:    &artifact.Fetcher{HTTPClient: http.DefaultClient, URLValidator: artifact.PermissiveHTTPURL, IPValidator: artifact.PermissiveIP},
		Now:        func() time.Time { return now },
	}
	_, _ = driveReconcile(r, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ss)})
}

func tm(s string) *metav1.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return &metav1.Time{Time: t}
}

// A Deny window active now holds the (first) rollout: nothing applies, the
// StageSet reports UpdateDeferred, and the held revision is on status.
func TestReconcile_UpdateWindow_DeniesRollout(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	servedArtifact(t, c, ns, "ea", "", map[string]string{"cm.yaml": configMapManifest(ns, "gated")})

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "denied"},
		Spec: stagesv1.StageSetSpec{
			Interval:      metav1.Duration{Duration: time.Minute},
			UpdateWindows: []stagesv1.UpdateWindow{{Type: window.TypeDeny, From: tm("2020-01-01T00:00:00Z"), To: tm("2030-01-01T00:00:00Z")}},
			Stages:        []stagesv1.Stage{{Name: "stage-a", SourceRef: stagesv1.SourceReference{Name: "ea"}}},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	reconcileAt(t, c, ss, tm("2025-06-01T00:00:00Z").Time)

	if cmExists(t, c, ns, "gated") {
		t.Fatal("a closed window must hold the rollout")
	}
	got := getStageSet(t, c, ns, "denied")
	if readyReason(got) != ReasonUpdateDeferred {
		t.Fatalf("Ready reason = %q, want %q", readyReason(got), ReasonUpdateDeferred)
	}
	if got.Status.PendingUpdate == nil || len(got.Status.PendingUpdate.Revisions) == 0 {
		t.Fatal("status.pendingUpdate should record the held revision")
	}
}

// An active Allow window permits the rollout.
func TestReconcile_UpdateWindow_AllowsWhenOpen(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	servedArtifact(t, c, ns, "ea", "", map[string]string{"cm.yaml": configMapManifest(ns, "open")})

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "allowed"},
		Spec: stagesv1.StageSetSpec{
			Interval:      metav1.Duration{Duration: time.Minute},
			UpdateWindows: []stagesv1.UpdateWindow{{Type: window.TypeAllow, From: tm("2020-01-01T00:00:00Z"), To: tm("2030-01-01T00:00:00Z")}},
			Stages:        []stagesv1.Stage{{Name: "stage-a", SourceRef: stagesv1.SourceReference{Name: "ea"}}},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	reconcileAt(t, c, ss, tm("2025-06-01T00:00:00Z").Time)

	if !cmExists(t, c, ns, "open") {
		t.Fatal("an active allow window should permit the rollout")
	}
	if r := readyReason(getStageSet(t, c, ns, "allowed")); r != ReasonReady {
		t.Fatalf("Ready reason = %q, want %q", r, ReasonReady)
	}
}

// The update-now override forces a held rollout through despite a Deny window.
func TestReconcile_UpdateWindow_OverrideForcesApply(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	servedArtifact(t, c, ns, "ea", "", map[string]string{"cm.yaml": configMapManifest(ns, "forced")})

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   ns,
			Name:        "override",
			Annotations: map[string]string{updateNowAnnotation: "tok-1"},
		},
		Spec: stagesv1.StageSetSpec{
			Interval:      metav1.Duration{Duration: time.Minute},
			UpdateWindows: []stagesv1.UpdateWindow{{Type: window.TypeDeny, From: tm("2020-01-01T00:00:00Z"), To: tm("2030-01-01T00:00:00Z")}},
			Stages:        []stagesv1.Stage{{Name: "stage-a", SourceRef: stagesv1.SourceReference{Name: "ea"}}},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	reconcileAt(t, c, ss, tm("2025-06-01T00:00:00Z").Time)

	if !cmExists(t, c, ns, "forced") {
		t.Fatal("update-now should force the rollout through a Deny window")
	}
	if v := getStageSet(t, c, ns, "override").Status.LastHandledUpdateOverride; v != "tok-1" {
		t.Fatalf("override token should be recorded, got %q", v)
	}
}

// An already-deployed StageSet whose update is held stays Ready=True; the old
// object remains and the new revision is recorded as pending.
func TestReconcile_UpdateWindow_DeployedStaysReadyWhenHeld(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	servedArtifact(t, c, ns, "ea", "", map[string]string{"cm.yaml": cmValManifest(ns, "rolling", "v1")})

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "held"},
		Spec: stagesv1.StageSetSpec{
			Interval:      metav1.Duration{Duration: time.Minute},
			UpdateWindows: []stagesv1.UpdateWindow{{Type: window.TypeAllow, From: tm("2020-01-01T00:00:00Z"), To: tm("2024-01-01T00:00:00Z")}},
			Stages:        []stagesv1.Stage{{Name: "stage-a", SourceRef: stagesv1.SourceReference{Name: "ea"}}},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	// Window open (2023) → initial deploy succeeds.
	reconcileAt(t, c, ss, tm("2023-06-01T00:00:00Z").Time)
	if cmDataKey(t, c, ns, "rolling") != "v1" {
		t.Fatal("initial deploy in an open window should apply v1")
	}

	// New revision, but the window is now closed (2025).
	repointArtifact(t, c, ns, "ea", map[string]string{"cm.yaml": cmValManifest(ns, "rolling", "v2")})
	reconcileAt(t, c, ss, tm("2025-06-01T00:00:00Z").Time)

	got := getStageSet(t, c, ns, "held")
	if readyReason(got) != ReasonReady {
		t.Fatalf("a deployed StageSet with a held update should stay Ready, reason = %q", readyReason(got))
	}
	if got.Status.PendingUpdate == nil {
		t.Fatal("the held new revision should be recorded in status.pendingUpdate")
	}
	if v := cmDataKey(t, c, ns, "rolling"); v != "v1" {
		t.Fatalf("the held update must not apply; want v1, got %q", v)
	}
}
