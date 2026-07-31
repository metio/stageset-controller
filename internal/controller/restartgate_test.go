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

	// promotedAt is the status of a stage whose last promotion recorded the given
	// per-check totals. Most cases below promote at zero, so a pod's current count
	// reads entirely as restarts since the stage was last known good.
	promotedAt := func(revision string, at map[string]int32) stagesv1.StageStatus {
		bl := make([]stagesv1.RestartCheckBaseline, 0, len(at))
		for name, n := range at {
			bl = append(bl, stagesv1.RestartCheckBaseline{Name: name, Restarts: n})
		}
		return stagesv1.StageStatus{AppliedRevision: revision, PromotionState: &stagesv1.PromotionState{RestartBaseline: bl}}
	}
	zeroed := func(names ...string) stagesv1.StageStatus {
		at := make(map[string]int32, len(names))
		for _, n := range names {
			at[n] = 0
		}
		return promotedAt("rev-1", at)
	}
	// neverGated is a stage that has never been through the restart gate.
	neverGated := stagesv1.StageStatus{}

	t.Run("no breach when within tolerance, and only selected pods count", func(t *testing.T) {
		c := build(
			pod("api-1", map[string]string{"app": "api"}, 0),
			pod("web-1", map[string]string{"app": "web"}, 9), // not selected → ignored
		)
		obs, err := r.evaluateRestartChecks(context.Background(), c, ss, stageWith(apiCheck(0)), zeroed("api"))
		if err != nil {
			t.Fatal(err)
		}
		if obs.verdict != nil {
			t.Fatalf("verdict=%+v, want nil (web pod must not count)", obs.verdict)
		}
	})

	t.Run("terminating pods are ignored", func(t *testing.T) {
		now := metav1.Now()
		old := pod("api-old", map[string]string{"app": "api"}, 9) // prior revision draining out
		old.DeletionTimestamp = &now
		old.Finalizers = []string{"stages.metio.wtf/test"} // fake client keeps DeletionTimestamp only with a finalizer
		c := build(old, pod("api-1", map[string]string{"app": "api"}, 0))
		obs, err := r.evaluateRestartChecks(context.Background(), c, ss, stageWith(apiCheck(0)), zeroed("api"))
		if err != nil {
			t.Fatal(err)
		}
		if obs.verdict != nil {
			t.Fatalf("verdict=%+v, want nil (terminating pod's restarts must not gate)", obs.verdict)
		}
	})

	t.Run("breach sums restarts across selected pods", func(t *testing.T) {
		c := build(
			pod("api-1", map[string]string{"app": "api"}, 2),
			pod("api-2", map[string]string{"app": "api"}, 1),
		)
		obs, err := r.evaluateRestartChecks(context.Background(), c, ss, stageWith(apiCheck(0)), zeroed("api"))
		if err != nil {
			t.Fatal(err)
		}
		if obs.verdict == nil || obs.verdict.check != "api" || obs.verdict.observed != 3 {
			t.Fatalf("verdict=%+v, want api/3", obs.verdict)
		}
	})

	t.Run("at the limit is not a breach", func(t *testing.T) {
		c := build(pod("api-1", map[string]string{"app": "api"}, 2))
		obs, err := r.evaluateRestartChecks(context.Background(), c, ss, stageWith(apiCheck(2)), zeroed("api"))
		if err != nil {
			t.Fatal(err)
		}
		if obs.verdict != nil {
			t.Fatalf("verdict=%+v, want nil (count == limit)", obs.verdict)
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
		obs, err := r.evaluateRestartChecks(context.Background(), c, ss, stage, zeroed("api", "worker"))
		if err != nil {
			t.Fatal(err)
		}
		if obs.verdict == nil || obs.verdict.check != "worker" || obs.verdict.observed != 5 {
			t.Fatalf("verdict=%+v, want worker/5", obs.verdict)
		}
		// Every check's baseline is recorded, not just the breaching one, or the
		// checks after a breach would re-baseline on the next pass.
		if len(obs.baseline) != 2 || len(obs.totals) != 2 {
			t.Fatalf("baseline=%+v totals=%+v, want an entry per check", obs.baseline, obs.totals)
		}
	})

	t.Run("gate onFailure default drives rollback", func(t *testing.T) {
		c := build(pod("api-1", map[string]string{"app": "api"}, 1))
		obs, err := r.evaluateRestartChecks(context.Background(), c, ss, stageGate(&stagesv1.RestartGate{
			OnFailure: "Rollback",
			Checks:    []stagesv1.RestartCheck{apiCheck(0)},
		}), zeroed("api"))
		if err != nil {
			t.Fatal(err)
		}
		if obs.verdict == nil || !obs.verdict.rollback {
			t.Fatalf("verdict=%+v, want rollback from gate default", obs.verdict)
		}
	})

	t.Run("per-check onFailure overrides the gate default", func(t *testing.T) {
		c := build(pod("api-1", map[string]string{"app": "api"}, 1))
		check := apiCheck(0)
		check.OnFailure = "Hold"
		obs, err := r.evaluateRestartChecks(context.Background(), c, ss, stageGate(&stagesv1.RestartGate{
			OnFailure: "Rollback",
			Checks:    []stagesv1.RestartCheck{check},
		}), zeroed("api"))
		if err != nil {
			t.Fatal(err)
		}
		if obs.verdict == nil || obs.verdict.rollback {
			t.Fatalf("verdict=%+v, want rollback=false (per-check Hold overrides)", obs.verdict)
		}
	})

	// The bug the baseline exists for: pods commonly survive a rollout and carry
	// every restart they have ever had. Charging those to the stage the first time
	// the gate looks fails a deploy over history it had no part in — and under
	// onFailure: Rollback reverts a revision that is fine.
	t.Run("a never-gated stage baselines instead of charging pod history", func(t *testing.T) {
		c := build(pod("api-1", map[string]string{"app": "api"}, 9))
		obs, err := r.evaluateRestartChecks(context.Background(), c, ss, stageWith(apiCheck(0)), neverGated)
		if err != nil {
			t.Fatal(err)
		}
		if obs.verdict != nil {
			t.Fatalf("verdict=%+v, want nil — restarts predating the gate must not fail it", obs.verdict)
		}
		if len(obs.baseline) != 1 || obs.baseline[0].Name != "api" || obs.baseline[0].Restarts != 9 {
			t.Fatalf("baseline=%+v, want [{api 9}]", obs.baseline)
		}
	})

	t.Run("restarts accrued since the baseline breach", func(t *testing.T) {
		c := build(pod("api-1", map[string]string{"app": "api"}, 12))
		obs, err := r.evaluateRestartChecks(context.Background(), c, ss, stageWith(apiCheck(2)),
			promotedAt("rev-1", map[string]int32{"api": 9}))
		if err != nil {
			t.Fatal(err)
		}
		if obs.verdict == nil || obs.verdict.observed != 3 {
			t.Fatalf("verdict=%+v, want a breach observing the 3 restarts since the baseline", obs.verdict)
		}
	})

	// The window is "since the last promotion", so it spans revisions. Keying it to
	// the revision instead would re-baseline on the very pass a new revision
	// arrives, and no breach could ever land on the pass that matters.
	t.Run("the baseline spans a revision change", func(t *testing.T) {
		c := build(pod("api-1", map[string]string{"app": "api"}, 3))
		obs, err := r.evaluateRestartChecks(context.Background(), c, ss, stageWith(apiCheck(0)),
			promotedAt("rev-0", map[string]int32{"api": 0})) // promoted at rev-0; rev-1 is landing now
		if err != nil {
			t.Fatal(err)
		}
		if obs.verdict == nil || obs.verdict.observed != 3 {
			t.Fatalf("verdict=%+v, want a breach — a new revision inherits the last promotion's baseline", obs.verdict)
		}
	})

	// Pods replaced since the baseline reset their counters, so the live total can
	// fall below it. That is not negative progress towards the limit.
	t.Run("a total below the baseline clamps to zero", func(t *testing.T) {
		c := build(pod("api-1", map[string]string{"app": "api"}, 1))
		obs, err := r.evaluateRestartChecks(context.Background(), c, ss, stageWith(apiCheck(0)),
			promotedAt("rev-1", map[string]int32{"api": 9}))
		if err != nil {
			t.Fatal(err)
		}
		if obs.verdict != nil {
			t.Fatalf("verdict=%+v, want nil — a replaced pod's reset counter is not a breach", obs.verdict)
		}
	})

	// The live totals travel back so the caller can adopt them as the next
	// baseline once the stage promotes.
	t.Run("live totals are reported alongside the baseline", func(t *testing.T) {
		c := build(pod("api-1", map[string]string{"app": "api"}, 7))
		obs, err := r.evaluateRestartChecks(context.Background(), c, ss, stageWith(apiCheck(99)),
			promotedAt("rev-1", map[string]int32{"api": 2}))
		if err != nil {
			t.Fatal(err)
		}
		if len(obs.totals) != 1 || obs.totals[0].Restarts != 7 {
			t.Fatalf("totals=%+v, want the live total 7", obs.totals)
		}
		if len(obs.baseline) != 1 || obs.baseline[0].Restarts != 2 {
			t.Fatalf("baseline=%+v, want the carried baseline 2", obs.baseline)
		}
	})
}
