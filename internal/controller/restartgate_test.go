// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

func TestEvaluateRestartChecks(t *testing.T) {
	const ns = "apps"
	pod := func(name string, labels map[string]string, restarts int32) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
			Status: corev1.PodStatus{
				InitContainerStatuses: []corev1.ContainerStatus{{Name: "init", RestartCount: 0}},
				ContainerStatuses:     []corev1.ContainerStatus{{Name: "app", RestartCount: restarts}},
			},
		}
	}
	ss := &stagesv1.StageSet{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: ns}}
	stageWith := func(checks ...stagesv1.RestartCheck) *stagesv1.Stage {
		return &stagesv1.Stage{Name: "staging", Promotion: &stagesv1.StagePromotion{RestartGate: &stagesv1.RestartGate{Checks: checks}}}
	}
	stageGate := func(gate *stagesv1.RestartGate) *stagesv1.Stage {
		return &stagesv1.Stage{Name: "staging", Promotion: &stagesv1.StagePromotion{RestartGate: gate}}
	}
	apiCheck := func(max int32) stagesv1.RestartCheck {
		return stagesv1.RestartCheck{
			Name:        "api",
			Selector:    metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}},
			MaxRestarts: max,
		}
	}
	r := &StageSetReconciler{}
	build := func(objs ...client.Object) client.Client {
		return fake.NewClientBuilder().WithScheme(builderScheme(t)).WithObjects(objs...).Build()
	}

	t.Run("no breach when within tolerance, and only selected pods count", func(t *testing.T) {
		c := build(
			pod("api-1", map[string]string{"app": "api"}, 0),
			pod("web-1", map[string]string{"app": "web"}, 9), // not selected → ignored
		)
		v, err := r.evaluateRestartChecks(context.Background(), c, ss, stageWith(apiCheck(0)))
		if err != nil {
			t.Fatal(err)
		}
		if v != nil {
			t.Fatalf("verdict=%+v, want nil (web pod must not count)", v)
		}
	})

	t.Run("terminating pods are ignored", func(t *testing.T) {
		now := metav1.Now()
		old := pod("api-old", map[string]string{"app": "api"}, 9) // prior revision draining out
		old.DeletionTimestamp = &now
		old.Finalizers = []string{"stages.metio.wtf/test"} // fake client keeps DeletionTimestamp only with a finalizer
		c := build(old, pod("api-1", map[string]string{"app": "api"}, 0))
		v, err := r.evaluateRestartChecks(context.Background(), c, ss, stageWith(apiCheck(0)))
		if err != nil {
			t.Fatal(err)
		}
		if v != nil {
			t.Fatalf("verdict=%+v, want nil (terminating pod's restarts must not gate)", v)
		}
	})

	t.Run("breach sums restarts across selected pods", func(t *testing.T) {
		c := build(
			pod("api-1", map[string]string{"app": "api"}, 2),
			pod("api-2", map[string]string{"app": "api"}, 1),
		)
		v, err := r.evaluateRestartChecks(context.Background(), c, ss, stageWith(apiCheck(0)))
		if err != nil {
			t.Fatal(err)
		}
		if v == nil || v.check != "api" || v.observed != 3 {
			t.Fatalf("verdict=%+v, want api/3", v)
		}
	})

	t.Run("at the limit is not a breach", func(t *testing.T) {
		c := build(pod("api-1", map[string]string{"app": "api"}, 2))
		v, err := r.evaluateRestartChecks(context.Background(), c, ss, stageWith(apiCheck(2)))
		if err != nil {
			t.Fatal(err)
		}
		if v != nil {
			t.Fatalf("verdict=%+v, want nil (count == limit)", v)
		}
	})

	t.Run("first breaching group wins across multiple checks", func(t *testing.T) {
		c := build(
			pod("api-1", map[string]string{"app": "api"}, 0),
			pod("worker-1", map[string]string{"app": "worker"}, 5),
		)
		stage := stageWith(
			apiCheck(0),
			stagesv1.RestartCheck{Name: "worker", Selector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "worker"}}, MaxRestarts: 2},
		)
		v, err := r.evaluateRestartChecks(context.Background(), c, ss, stage)
		if err != nil {
			t.Fatal(err)
		}
		if v == nil || v.check != "worker" || v.observed != 5 {
			t.Fatalf("verdict=%+v, want worker/5", v)
		}
	})

	t.Run("gate onFailure default drives rollback", func(t *testing.T) {
		c := build(pod("api-1", map[string]string{"app": "api"}, 1))
		v, err := r.evaluateRestartChecks(context.Background(), c, ss, stageGate(&stagesv1.RestartGate{
			OnFailure: "Rollback",
			Checks:    []stagesv1.RestartCheck{apiCheck(0)},
		}))
		if err != nil {
			t.Fatal(err)
		}
		if v == nil || !v.rollback {
			t.Fatalf("verdict=%+v, want rollback from gate default", v)
		}
	})

	t.Run("per-check onFailure overrides the gate default", func(t *testing.T) {
		c := build(pod("api-1", map[string]string{"app": "api"}, 1))
		check := apiCheck(0)
		check.OnFailure = "Hold"
		v, err := r.evaluateRestartChecks(context.Background(), c, ss, stageGate(&stagesv1.RestartGate{
			OnFailure: "Rollback",
			Checks:    []stagesv1.RestartCheck{check},
		}))
		if err != nil {
			t.Fatal(err)
		}
		if v == nil || v.rollback {
			t.Fatalf("verdict=%+v, want rollback=false (per-check Hold overrides)", v)
		}
	})
}
