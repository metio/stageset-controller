// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

func TestEvaluateEventChecks(t *testing.T) {
	const ns = "apps"
	pod := func(name, uid string, labels map[string]string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: types.UID(uid), Labels: labels}}
	}
	evt := func(name, podUID, reason, typ string, count int32) *corev1.Event {
		return &corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Name: name, Namespace: ns},
			InvolvedObject: corev1.ObjectReference{Kind: "Pod", Namespace: ns, UID: types.UID(podUID)},
			Reason:         reason,
			Type:           typ,
			Count:          count,
		}
	}
	ss := &stagesv1.StageSet{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: ns}}
	gateStage := func(gate *stagesv1.EventGate) *stagesv1.Stage {
		return &stagesv1.Stage{Name: "staging", Promotion: &stagesv1.StagePromotion{EventGate: gate}}
	}
	apiCheck := func(max int32, reasons ...string) stagesv1.EventCheck {
		return stagesv1.EventCheck{
			Name:      "api",
			Selector:  metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}},
			Reasons:   reasons,
			MaxEvents: max,
		}
	}
	r := &StageSetReconciler{}
	build := func(objs ...client.Object) client.Client {
		return fake.NewClientBuilder().WithScheme(builderScheme(t)).WithObjects(objs...).Build()
	}
	apiPod := pod("api-1", "uid-api", map[string]string{"app": "api"})

	t.Run("matching warning events over the limit breach", func(t *testing.T) {
		c := build(apiPod, evt("e1", "uid-api", "FailedScheduling", "Warning", 3))
		v, err := r.evaluateEventChecks(context.Background(), c, ss, gateStage(&stagesv1.EventGate{Checks: []stagesv1.EventCheck{apiCheck(0, "FailedScheduling")}}))
		if err != nil {
			t.Fatal(err)
		}
		if v == nil || v.check != "api" || v.observed != 3 {
			t.Fatalf("verdict=%+v, want api/3", v)
		}
	})

	t.Run("non-allowed reason, Normal type, and other pods are ignored", func(t *testing.T) {
		c := build(
			apiPod,
			pod("web-1", "uid-web", map[string]string{"app": "web"}),
			evt("e1", "uid-api", "Unhealthy", "Warning", 9),        // reason not allow-listed
			evt("e2", "uid-api", "FailedScheduling", "Normal", 9),  // not a Warning
			evt("e3", "uid-web", "FailedScheduling", "Warning", 9), // a different pod
		)
		v, err := r.evaluateEventChecks(context.Background(), c, ss, gateStage(&stagesv1.EventGate{Checks: []stagesv1.EventCheck{apiCheck(0, "FailedScheduling")}}))
		if err != nil {
			t.Fatal(err)
		}
		if v != nil {
			t.Fatalf("verdict=%+v, want nil", v)
		}
	})

	t.Run("events on a previous pod incarnation (other UID) do not count", func(t *testing.T) {
		c := build(apiPod, evt("e1", "uid-OLD", "FailedScheduling", "Warning", 9))
		v, err := r.evaluateEventChecks(context.Background(), c, ss, gateStage(&stagesv1.EventGate{Checks: []stagesv1.EventCheck{apiCheck(0, "FailedScheduling")}}))
		if err != nil {
			t.Fatal(err)
		}
		if v != nil {
			t.Fatalf("verdict=%+v, want nil (stale UID)", v)
		}
	})

	t.Run("events on a terminating pod are ignored", func(t *testing.T) {
		now := metav1.Now()
		old := pod("api-old", "uid-old", map[string]string{"app": "api"})
		old.DeletionTimestamp = &now
		old.Finalizers = []string{"stages.metio.wtf/test"} // fake client keeps DeletionTimestamp only with a finalizer
		c := build(old, evt("e1", "uid-old", "FailedScheduling", "Warning", 9))
		v, err := r.evaluateEventChecks(context.Background(), c, ss, gateStage(&stagesv1.EventGate{Checks: []stagesv1.EventCheck{apiCheck(0, "FailedScheduling")}}))
		if err != nil {
			t.Fatal(err)
		}
		if v != nil {
			t.Fatalf("verdict=%+v, want nil (terminating pod's events must not gate)", v)
		}
	})

	t.Run("at the limit is not a breach", func(t *testing.T) {
		c := build(apiPod, evt("e1", "uid-api", "FailedScheduling", "Warning", 2))
		v, err := r.evaluateEventChecks(context.Background(), c, ss, gateStage(&stagesv1.EventGate{Checks: []stagesv1.EventCheck{apiCheck(2, "FailedScheduling")}}))
		if err != nil {
			t.Fatal(err)
		}
		if v != nil {
			t.Fatalf("verdict=%+v, want nil (count == limit)", v)
		}
	})

	t.Run("gate onFailure default and per-check override", func(t *testing.T) {
		c := build(apiPod, evt("e1", "uid-api", "OOMKilling", "Warning", 1))
		v, err := r.evaluateEventChecks(context.Background(), c, ss, gateStage(&stagesv1.EventGate{
			OnFailure: "Rollback",
			Checks:    []stagesv1.EventCheck{apiCheck(0, "OOMKilling")},
		}))
		if err != nil {
			t.Fatal(err)
		}
		if v == nil || !v.rollback {
			t.Fatalf("verdict=%+v, want rollback from gate default", v)
		}
		ck := apiCheck(0, "OOMKilling")
		ck.OnFailure = "Hold"
		v, err = r.evaluateEventChecks(context.Background(), c, ss, gateStage(&stagesv1.EventGate{
			OnFailure: "Rollback",
			Checks:    []stagesv1.EventCheck{ck},
		}))
		if err != nil {
			t.Fatal(err)
		}
		if v == nil || v.rollback {
			t.Fatalf("verdict=%+v, want rollback=false (per-check Hold)", v)
		}
	})
}
