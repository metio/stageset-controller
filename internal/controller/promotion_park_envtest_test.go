// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/artifact"
)

// countEvents returns how many recorded events carry the reason and contain
// substr in their note.
func (c *capturingRecorder) countEvents(reason, substr string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, e := range c.events {
		if e.reason == reason && strings.Contains(e.note, substr) {
			n++
		}
	}
	return n
}

// breachingPod creates a pod matching app=api whose container has restarted —
// the restart gate's breach signal. The status subresource carries the counter.
func breachingPod(t *testing.T, c client.Client, ns, name string, restarts int32) {
	t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: map[string]string{"app": "api"}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "app", Image: "registry.example/app:1",
		}}},
	}
	if err := c.Create(context.Background(), pod); err != nil {
		t.Fatalf("create pod: %v", err)
	}
	pod.Status = corev1.PodStatus{
		ContainerStatuses: []corev1.ContainerStatus{{
			Name: "app", Image: "registry.example/app:1", RestartCount: restarts,
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}},
		}},
	}
	if err := c.Status().Update(context.Background(), pod); err != nil {
		t.Fatalf("update pod status: %v", err)
	}
}

// TestReconcile_GateRollback_ParksAbortedRevision drives the restart gate's
// onFailure=Rollback end-to-end: the breach reverts the stage to its last-good
// revision, and the aborted revision must then PARK — not be re-applied (and
// re-rolled-back, or worse, promoted once the replacement pods read clean) on
// every subsequent reconcile. A revision change and a fresh promote token both
// un-park.
func TestReconcile_GateRollback_ParksAbortedRevision(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	servedArtifact(t, c, ns, "ea", "", map[string]string{"cm.yaml": cmValManifest(ns, "parked", "v1")})

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "gate-park"},
		Spec: stagesv1.StageSetSpec{
			Interval:          metav1.Duration{Duration: time.Minute},
			RollbackOnFailure: true,
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
	newReconciler := func() *StageSetReconciler {
		return &StageSetReconciler{
			Client:     c,
			RESTMapper: c.RESTMapper(),
			Recorder:   rec,
			Fetcher:    &artifact.Fetcher{HTTPClient: http.DefaultClient, URLValidator: artifact.PermissiveHTTPURL, IPValidator: artifact.PermissiveIP},
		}
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "gate-park"}}

	// Pass 1: no pods → the gate is clean, v1 promotes, the snapshot records v1.
	if _, err := driveReconcile(newReconciler(), req); err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	if got := readyReason(getStageSet(t, c, ns, "gate-park")); got != ReasonReady {
		t.Fatalf("pass 1 Ready reason = %q, want %q", got, ReasonReady)
	}

	// v2 arrives while the watched pods are crash-looping.
	repointArtifact(t, c, ns, "ea", map[string]string{"cm.yaml": cmValManifest(ns, "parked", "v2")})
	breachingPod(t, c, ns, "api-crashy", 3)

	// Pass 2: v2 applies, the gate breaches, onFailure=Rollback reverts to v1 and
	// records the aborted revision.
	if _, err := driveReconcile(newReconciler(), req); err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	after2 := getStageSet(t, c, ns, "gate-park")
	if v := cmDataKey(t, c, ns, "parked"); v != "v1" {
		t.Fatalf("pass 2 should roll back to v1, got %q", v)
	}
	sa := stageStatusFor(after2, "stage-a")
	if sa == nil || sa.PromotionState == nil || sa.PromotionState.AbortedRevision == "" {
		t.Fatalf("pass 2 should record the aborted revision, got %+v", sa)
	}
	abortedRev := sa.PromotionState.AbortedRevision
	if n := rec.countEvents(ReasonPromotionBlocked, "failed; rolled back to revision"); n != 1 {
		t.Fatalf("pass 2 should emit exactly one rolled-back event, got %d", n)
	}
	var cmAfter2 corev1.ConfigMap
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "parked"}, &cmAfter2); err != nil {
		t.Fatalf("get cm: %v", err)
	}

	// Pass 3 (same revision still pinned): the abort must PARK. No re-apply of
	// v2, no second rollback, the ConfigMap untouched.
	if _, err := driveReconcile(newReconciler(), req); err != nil {
		t.Fatalf("pass 3: %v", err)
	}
	if v := cmDataKey(t, c, ns, "parked"); v != "v1" {
		t.Fatalf("pass 3 re-applied the aborted revision; ConfigMap = %q, want v1", v)
	}
	var cmAfter3 corev1.ConfigMap
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "parked"}, &cmAfter3); err != nil {
		t.Fatalf("get cm: %v", err)
	}
	if cmAfter3.ResourceVersion != cmAfter2.ResourceVersion {
		t.Fatalf("pass 3 rewrote the stage's object (rv %s → %s): the aborted revision was re-applied instead of parked", cmAfter2.ResourceVersion, cmAfter3.ResourceVersion)
	}
	if n := rec.countEvents(ReasonPromotionBlocked, "failed; rolled back to revision"); n != 1 {
		t.Fatalf("pass 3 rolled back again (%d events, want 1): the aborted revision was re-applied instead of parked", n)
	}
	after3 := getStageSet(t, c, ns, "gate-park")
	if sa := stageStatusFor(after3, "stage-a"); sa == nil || sa.PromotionState == nil || sa.PromotionState.AbortedRevision != abortedRev {
		t.Fatalf("the park must keep the aborted-revision record, got %+v", sa)
	}

	// A NEW revision un-parks: v3 is attempted (and, still breaching, aborts
	// again — the recorded aborted revision moves to v3's).
	v3rev := repointArtifact(t, c, ns, "ea", map[string]string{"cm.yaml": cmValManifest(ns, "parked", "v3")})
	if _, err := driveReconcile(newReconciler(), req); err != nil {
		t.Fatalf("pass 4: %v", err)
	}
	after4 := getStageSet(t, c, ns, "gate-park")
	if sa := stageStatusFor(after4, "stage-a"); sa == nil || sa.PromotionState == nil || sa.PromotionState.AbortedRevision != v3rev {
		t.Fatalf("a new revision must un-park and be judged on its own; abortedRevision = %+v, want %q", sa, v3rev)
	}

	// A fresh promote token un-parks AND break-glasses the still-breaching gate:
	// v3 lands.
	cur := getStageSet(t, c, ns, "gate-park")
	if cur.Annotations == nil {
		cur.Annotations = map[string]string{}
	}
	cur.Annotations[promoteAnnotation] = "stage-a@ship-it"
	if err := c.Update(context.Background(), cur); err != nil {
		t.Fatalf("annotate promote: %v", err)
	}
	if _, err := driveReconcile(newReconciler(), req); err != nil {
		t.Fatalf("pass 5: %v", err)
	}
	if v := cmDataKey(t, c, ns, "parked"); v != "v3" {
		t.Fatalf("a fresh promote token must un-park and ship v3, got %q", v)
	}
	if got := readyReason(getStageSet(t, c, ns, "gate-park")); got != ReasonReady {
		t.Fatalf("pass 5 Ready reason = %q, want %q", got, ReasonReady)
	}
}

// TestReconcile_EventGateRollback_ParksAbortedRevision covers the same park for
// an EVENT-gate abort: the second rollback-capable gate must behave identically.
func TestReconcile_EventGateRollback_ParksAbortedRevision(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	servedArtifact(t, c, ns, "ea", "", map[string]string{"cm.yaml": cmValManifest(ns, "ev-parked", "v1")})

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "event-park"},
		Spec: stagesv1.StageSetSpec{
			Interval:          metav1.Duration{Duration: time.Minute},
			RollbackOnFailure: true,
			Stages: []stagesv1.Stage{{
				Name:      "stage-a",
				SourceRef: stagesv1.SourceReference{Name: "ea"},
				Promotion: &stagesv1.StagePromotion{EventGate: &stagesv1.EventGate{
					OnFailure: "Rollback",
					Checks: []stagesv1.EventCheck{{
						Name:      "api",
						Selector:  metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}},
						Reasons:   []string{"BackOff"},
						MaxEvents: 0,
					}},
				}},
			}},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	rec := &capturingRecorder{}
	newReconciler := func() *StageSetReconciler {
		return &StageSetReconciler{
			Client:     c,
			RESTMapper: c.RESTMapper(),
			Recorder:   rec,
			Fetcher:    &artifact.Fetcher{HTTPClient: http.DefaultClient, URLValidator: artifact.PermissiveHTTPURL, IPValidator: artifact.PermissiveIP},
		}
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "event-park"}}

	// Pass 1: clean, v1 lands, snapshot recorded.
	if _, err := driveReconcile(newReconciler(), req); err != nil {
		t.Fatalf("pass 1: %v", err)
	}

	// v2 arrives while a watched pod spews Warning events.
	repointArtifact(t, c, ns, "ea", map[string]string{"cm.yaml": cmValManifest(ns, "ev-parked", "v2")})
	breachingPod(t, c, ns, "api-noisy", 0)
	var noisy corev1.Pod
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "api-noisy"}, &noisy); err != nil {
		t.Fatalf("get pod: %v", err)
	}
	ev := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "api-noisy.warn1"},
		InvolvedObject: corev1.ObjectReference{
			APIVersion: "v1", Kind: "Pod", Namespace: ns, Name: "api-noisy", UID: noisy.UID,
		},
		Type: corev1.EventTypeWarning, Reason: "BackOff", Message: "restarting", Count: 3,
	}
	if err := c.Create(context.Background(), ev); err != nil {
		t.Fatalf("create event: %v", err)
	}

	// Pass 2: v2 applies, the event gate breaches → rollback to v1 + abort record.
	if _, err := driveReconcile(newReconciler(), req); err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	if v := cmDataKey(t, c, ns, "ev-parked"); v != "v1" {
		t.Fatalf("pass 2 should roll back to v1, got %q", v)
	}
	after2 := getStageSet(t, c, ns, "event-park")
	sa := stageStatusFor(after2, "stage-a")
	if sa == nil || sa.PromotionState == nil || sa.PromotionState.AbortedRevision == "" {
		t.Fatalf("pass 2 should record the aborted revision, got %+v", sa)
	}
	if n := rec.countEvents(ReasonPromotionBlocked, "failed; rolled back to revision"); n != 1 {
		t.Fatalf("pass 2 should emit exactly one rolled-back event, got %d", n)
	}

	// Pass 3: the abort must park — no second rollback, object untouched.
	var cmAfter2 corev1.ConfigMap
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "ev-parked"}, &cmAfter2); err != nil {
		t.Fatalf("get cm: %v", err)
	}
	if _, err := driveReconcile(newReconciler(), req); err != nil {
		t.Fatalf("pass 3: %v", err)
	}
	if v := cmDataKey(t, c, ns, "ev-parked"); v != "v1" {
		t.Fatalf("pass 3 re-applied the aborted revision; ConfigMap = %q, want v1", v)
	}
	var cmAfter3 corev1.ConfigMap
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "ev-parked"}, &cmAfter3); err != nil {
		t.Fatalf("get cm: %v", err)
	}
	if cmAfter3.ResourceVersion != cmAfter2.ResourceVersion {
		t.Fatal("pass 3 rewrote the stage's object: an event-gate abort was re-applied instead of parked")
	}
	if n := rec.countEvents(ReasonPromotionBlocked, "failed; rolled back to revision"); n != 1 {
		t.Fatalf("pass 3 rolled back again (%d events, want 1): the event-gate abort did not park", n)
	}
}
