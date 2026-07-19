// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/metricsource"
)

// labeledNamespace creates a namespace carrying the given labels, so a
// namespaceSelector can be exercised.
func labeledNamespace(t *testing.T, c client.Client, labels map[string]string) string {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "fleet-ns-", Labels: labels}}
	if err := c.Create(context.Background(), ns); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	t.Cleanup(func() { _ = c.Delete(context.Background(), ns) })
	return ns.Name
}

// fleetMember creates a minimal fleet-managed StageSet (approvalMode: Always)
// carrying the given app + ring labels, deployed at 1.0.0. The FleetRollout
// reconciler only reads its labels and status and stamps its approval annotation;
// the StageSet reconciler is not run here. Each test uses a UNIQUE app value: the
// cluster-scoped fleet selects across all namespaces, and envtest does not finalize
// namespace deletion, so a shared label would let one test's fleet pick up another's
// leftover members.
func fleetMember(t *testing.T, c client.Client, ns, name, app, ring string) {
	t.Helper()
	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: map[string]string{"app": app, "ring": ring}},
		Spec: stagesv1.StageSetSpec{
			Interval: metav1.Duration{Duration: time.Minute},
			Version:  &stagesv1.VersionSource{Value: "1.0.0", ApprovalMode: stagesv1.ApprovalAlways},
			Stages:   []stagesv1.Stage{{Name: "app", SourceRef: stagesv1.SourceReference{Name: "ea"}}},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create member %s: %v", name, err)
	}
}

// settleMember simulates a member reaching the target version and going Ready at
// its current generation — what the StageSet reconciler would produce once the
// member's approval is granted and its source offers the version.
func settleMember(t *testing.T, c client.Client, ns, name, version string) {
	t.Helper()
	ss := getStageSet(t, c, ns, name)
	ss.Status.Version = version
	ss.Status.PendingVersion = "" // adopted: nothing awaiting approval
	ss.Status.ObservedGeneration = ss.Generation
	apimeta.SetStatusCondition(&ss.Status.Conditions, metav1.Condition{
		Type: ConditionReady, Status: metav1.ConditionTrue, Reason: ReasonReady,
		Message: "ok", ObservedGeneration: ss.Generation,
	})
	if err := c.Status().Update(context.Background(), ss); err != nil {
		t.Fatalf("settle member %s: %v", name, err)
	}
}

// holdMember simulates a member held awaiting approval for version — Ready=False,
// status.pendingVersion set — which is what the StageSet reconciler produces under
// approvalMode: Always with a pending advance the fleet has not yet approved.
func holdMember(t *testing.T, c client.Client, ns, name, version string) {
	t.Helper()
	ss := getStageSet(t, c, ns, name)
	ss.Status.PendingVersion = version
	ss.Status.ObservedGeneration = ss.Generation
	apimeta.SetStatusCondition(&ss.Status.Conditions, metav1.Condition{
		Type: ConditionReady, Status: metav1.ConditionFalse, Reason: ReasonAwaitingApproval,
		Message: "awaiting approval", ObservedGeneration: ss.Generation,
	})
	if err := c.Status().Update(context.Background(), ss); err != nil {
		t.Fatalf("hold member %s: %v", name, err)
	}
}

// unsettleMember makes a member unhealthy (Ready=False) — a regression after it
// had reached the target version.
func unsettleMember(t *testing.T, c client.Client, ns, name string) {
	t.Helper()
	ss := getStageSet(t, c, ns, name)
	apimeta.SetStatusCondition(&ss.Status.Conditions, metav1.Condition{
		Type: ConditionReady, Status: metav1.ConditionFalse, Reason: ReasonStageFailed,
		Message: "broke", ObservedGeneration: ss.Generation,
	})
	if err := c.Status().Update(context.Background(), ss); err != nil {
		t.Fatalf("unsettle member %s: %v", name, err)
	}
}

func reconcileFleet(t *testing.T, c client.Client, name string) ctrl.Result {
	t.Helper()
	r := &FleetRolloutReconciler{Client: c}
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}})
	if err != nil {
		t.Fatalf("fleet reconcile: %v", err)
	}
	return res
}

// reconcileFleetAt drives one fleet reconcile with an injected clock and metric
// querier, so soak timing and health gates are deterministic.
func reconcileFleetAt(t *testing.T, c client.Client, name string, now time.Time, q metricsource.Querier) {
	t.Helper()
	r := &FleetRolloutReconciler{Client: c, Now: func() time.Time { return now }, MetricQuerier: q}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}}); err != nil {
		t.Fatalf("fleet reconcile: %v", err)
	}
}

func getFleet(t *testing.T, c client.Client, name string) *stagesv1.FleetRollout {
	t.Helper()
	var fr stagesv1.FleetRollout
	if err := c.Get(context.Background(), types.NamespacedName{Name: name}, &fr); err != nil {
		t.Fatalf("get fleet %s: %v", name, err)
	}
	return &fr
}

func memberApproved(t *testing.T, c client.Client, ns, name string) string {
	t.Helper()
	return getStageSet(t, c, ns, name).Annotations[approvedVersionAnnotation]
}

func ringSelector(ring string) metav1.LabelSelector {
	return metav1.LabelSelector{MatchLabels: map[string]string{"ring": ring}}
}

func appSelector(app string) metav1.LabelSelector {
	return metav1.LabelSelector{MatchLabels: map[string]string{"app": app}}
}

func maxThreshold(v string) stagesv1.Threshold { return stagesv1.Threshold{Max: &v} }

// TestFleetRollout_OpensWavesInOrder drives the whole Phase-1 loop: wave 0 is
// approved first and wave 1 is held; only once wave 0 settles at the target does
// wave 1 open; once it settles the rollout completes.
func TestFleetRollout_OpensWavesInOrder(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	const app = "order"
	fleetMember(t, c, ns, "app0a", app, "0")
	fleetMember(t, c, ns, "app0b", app, "0")
	fleetMember(t, c, ns, "app1a", app, "1")

	fr := &stagesv1.FleetRollout{
		ObjectMeta: metav1.ObjectMeta{Name: "fleet-order"},
		Spec: stagesv1.FleetRolloutSpec{
			TargetVersion: "2.0.0",
			Selector:      appSelector(app),
			Waves: []stagesv1.FleetWave{
				{Name: "canary", Selector: ringSelector("0")},
				{Name: "broad", Selector: ringSelector("1")},
			},
		},
	}
	if err := c.Create(context.Background(), fr); err != nil {
		t.Fatalf("create FleetRollout: %v", err)
	}

	// First reconcile: wave 0 is approved, wave 1 held.
	reconcileFleet(t, c, "fleet-order")
	if got := memberApproved(t, c, ns, "app0a"); got != "2.0.0" {
		t.Fatalf("wave-0 member app0a should be approved to 2.0.0, got %q", got)
	}
	if got := memberApproved(t, c, ns, "app0b"); got != "2.0.0" {
		t.Fatalf("wave-0 member app0b should be approved, got %q", got)
	}
	if got := memberApproved(t, c, ns, "app1a"); got != "" {
		t.Fatalf("wave-1 member app1a must be held until wave 0 settles, got %q", got)
	}
	if fr := getFleet(t, c, "fleet-order"); fr.Status.Phase != stagesv1.FleetInProgress || fr.Status.CurrentWave != "canary" {
		t.Fatalf("phase/wave = %q/%q, want InProgress/canary", fr.Status.Phase, fr.Status.CurrentWave)
	}

	// Wave 0 reaches the target → wave 1 opens.
	settleMember(t, c, ns, "app0a", "2.0.0")
	settleMember(t, c, ns, "app0b", "2.0.0")
	reconcileFleet(t, c, "fleet-order")
	if got := memberApproved(t, c, ns, "app1a"); got != "2.0.0" {
		t.Fatalf("wave-1 member app1a should be approved once wave 0 settled, got %q", got)
	}
	if fr := getFleet(t, c, "fleet-order"); fr.Status.CurrentWave != "broad" {
		t.Fatalf("current wave = %q, want broad", fr.Status.CurrentWave)
	}

	// Wave 1 reaches the target → the rollout completes.
	settleMember(t, c, ns, "app1a", "2.0.0")
	reconcileFleet(t, c, "fleet-order")
	final := getFleet(t, c, "fleet-order")
	if final.Status.Phase != stagesv1.FleetCompleted {
		t.Fatalf("phase = %q, want Completed", final.Status.Phase)
	}
	if cond := apimeta.FindStatusCondition(final.Status.Conditions, ConditionReady); cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != ReasonFleetCompleted {
		t.Fatalf("Ready condition = %+v, want True/Completed", cond)
	}
}

// TestFleetRollout_DerivedTargetFromPendingVersion proves that with NO
// spec.targetVersion the fleet derives each member's target from its own held
// advance (status.pendingVersion) and paces it wave by wave — zero manual target.
func TestFleetRollout_DerivedTargetFromPendingVersion(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	const app = "derived"
	fleetMember(t, c, ns, "w0", app, "0")
	fleetMember(t, c, ns, "w1", app, "1")
	// Both members' sources offer 2.0.0, so each is held awaiting approval for it.
	holdMember(t, c, ns, "w0", "2.0.0")
	holdMember(t, c, ns, "w1", "2.0.0")

	fr := &stagesv1.FleetRollout{
		ObjectMeta: metav1.ObjectMeta{Name: "fleet-derived"},
		Spec: stagesv1.FleetRolloutSpec{
			// No TargetVersion — derived.
			Selector: appSelector(app),
			Waves: []stagesv1.FleetWave{
				{Name: "canary", Selector: ringSelector("0")},
				{Name: "broad", Selector: ringSelector("1")},
			},
		},
	}
	if err := c.Create(context.Background(), fr); err != nil {
		t.Fatalf("create FleetRollout: %v", err)
	}

	// Wave 0 approved to its own pending version; wave 1 held.
	reconcileFleet(t, c, "fleet-derived")
	if got := memberApproved(t, c, ns, "w0"); got != "2.0.0" {
		t.Fatalf("wave-0 member should be approved to its derived pending version 2.0.0, got %q", got)
	}
	if got := memberApproved(t, c, ns, "w1"); got != "" {
		t.Fatalf("wave-1 member must be held until wave 0 settles, got %q", got)
	}

	// Wave 0 adopts → wave 1 opens (also derived).
	settleMember(t, c, ns, "w0", "2.0.0")
	reconcileFleet(t, c, "fleet-derived")
	if got := memberApproved(t, c, ns, "w1"); got != "2.0.0" {
		t.Fatalf("wave-1 member should be approved once wave 0 settled, got %q", got)
	}

	// Wave 1 adopts → completed.
	settleMember(t, c, ns, "w1", "2.0.0")
	reconcileFleet(t, c, "fleet-derived")
	if got := getFleet(t, c, "fleet-derived"); got.Status.Phase != stagesv1.FleetCompleted {
		t.Fatalf("phase = %q, want Completed", got.Status.Phase)
	}
}

// TestFleetRollout_SoakHoldsThenAdvances proves a settled wave holds for its soak
// before the next wave opens, and advances once the soak elapses.
func TestFleetRollout_SoakHoldsThenAdvances(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	const app = "soak"
	fleetMember(t, c, ns, "w0", app, "0")
	fleetMember(t, c, ns, "w1", app, "1")

	soak := metav1.Duration{Duration: 30 * time.Minute}
	fr := &stagesv1.FleetRollout{
		ObjectMeta: metav1.ObjectMeta{Name: "fleet-soak"},
		Spec: stagesv1.FleetRolloutSpec{
			TargetVersion: "2.0.0",
			Selector:      appSelector(app),
			Waves: []stagesv1.FleetWave{
				{Name: "canary", Selector: ringSelector("0"), Soak: &soak},
				{Name: "broad", Selector: ringSelector("1")},
			},
		},
	}
	if err := c.Create(context.Background(), fr); err != nil {
		t.Fatalf("create FleetRollout: %v", err)
	}
	t0 := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)

	reconcileFleetAt(t, c, "fleet-soak", t0, nil) // opens wave 0
	settleMember(t, c, ns, "w0", "2.0.0")

	reconcileFleetAt(t, c, "fleet-soak", t0, nil) // wave 0 settled, now soaking
	if got := memberApproved(t, c, ns, "w1"); got != "" {
		t.Fatalf("wave 1 must stay held during wave 0 soak, got %q", got)
	}
	if fr := getFleet(t, c, "fleet-soak"); fr.Status.Waves[0].SoakUntil == nil {
		t.Fatal("a settled wave should have a soak deadline")
	}

	// Past the soak: wave 0 passes and wave 1 opens.
	reconcileFleetAt(t, c, "fleet-soak", t0.Add(31*time.Minute), nil)
	if got := memberApproved(t, c, ns, "w1"); got != "2.0.0" {
		t.Fatalf("wave 1 should open after wave 0 soaks, got %q", got)
	}
}

// TestFleetRollout_GateFailureHalts proves a wave whose health gate is violated
// halts the whole fleet.
func TestFleetRollout_GateFailureHalts(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	const app = "gate"
	fleetMember(t, c, ns, "w0", app, "0")
	fleetMember(t, c, ns, "w1", app, "1")

	fr := &stagesv1.FleetRollout{
		ObjectMeta: metav1.ObjectMeta{Name: "fleet-gate"},
		Spec: stagesv1.FleetRolloutSpec{
			TargetVersion: "2.0.0",
			Selector:      appSelector(app),
			Waves: []stagesv1.FleetWave{
				{Name: "canary", Selector: ringSelector("0"), Gate: &stagesv1.FleetWaveGate{Threshold: maxThreshold("0.01")}},
				{Name: "broad", Selector: ringSelector("1")},
			},
		},
	}
	if err := c.Create(context.Background(), fr); err != nil {
		t.Fatalf("create FleetRollout: %v", err)
	}
	t0 := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)

	reconcileFleetAt(t, c, "fleet-gate", t0, &fakeQuerier{value: 0.5})
	settleMember(t, c, ns, "w0", "2.0.0")

	// Wave 0 settled, soak is 0, gate metric 0.5 > 0.01 → Failing → halt.
	reconcileFleetAt(t, c, "fleet-gate", t0, &fakeQuerier{value: 0.5})
	got := getFleet(t, c, "fleet-gate")
	if got.Status.Phase != stagesv1.FleetHalted {
		t.Fatalf("phase = %q, want Halted", got.Status.Phase)
	}
	cond := apimeta.FindStatusCondition(got.Status.Conditions, ConditionReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != ReasonFleetHalted {
		t.Fatalf("Ready condition = %+v, want False/Halted", cond)
	}
	if got := memberApproved(t, c, ns, "w1"); got != "" {
		t.Fatalf("a halted fleet must not open wave 1, got %q", got)
	}
	if got.Status.Waves[0].Health != "Failing" {
		t.Fatalf("wave 0 health = %q, want Failing", got.Status.Waves[0].Health)
	}
}

// TestFleetRollout_MemberRegressionHalts proves a member that reaches the target
// then goes not-Ready halts the fleet.
func TestFleetRollout_MemberRegressionHalts(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	const app = "regress"
	fleetMember(t, c, ns, "w0", app, "0")
	fleetMember(t, c, ns, "w1", app, "1")

	soak := metav1.Duration{Duration: 30 * time.Minute}
	fr := &stagesv1.FleetRollout{
		ObjectMeta: metav1.ObjectMeta{Name: "fleet-regress"},
		Spec: stagesv1.FleetRolloutSpec{
			TargetVersion: "2.0.0",
			Selector:      appSelector(app),
			Waves: []stagesv1.FleetWave{
				{Name: "canary", Selector: ringSelector("0"), Soak: &soak},
				{Name: "broad", Selector: ringSelector("1")},
			},
		},
	}
	if err := c.Create(context.Background(), fr); err != nil {
		t.Fatalf("create FleetRollout: %v", err)
	}
	t0 := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)

	reconcileFleetAt(t, c, "fleet-regress", t0, nil)
	settleMember(t, c, ns, "w0", "2.0.0")
	reconcileFleetAt(t, c, "fleet-regress", t0, nil) // wave 0 settled + soaking

	// A member of the soaking wave breaks → the fleet halts.
	unsettleMember(t, c, ns, "w0")
	reconcileFleetAt(t, c, "fleet-regress", t0.Add(time.Minute), nil)
	got := getFleet(t, c, "fleet-regress")
	if got.Status.Phase != stagesv1.FleetHalted {
		t.Fatalf("phase = %q, want Halted", got.Status.Phase)
	}
	if cond := apimeta.FindStatusCondition(got.Status.Conditions, ConditionReady); cond == nil || cond.Reason != ReasonFleetHalted {
		t.Fatalf("Ready reason = %v, want Halted", cond)
	}
}

// TestFleetRollout_OnRegressionRollbackStampsMembers proves that with
// onRegression: Rollback a regression stamps the rollback-to annotation
// (previousVersion) on the halted wave's members — the directive that unwinds them.
func TestFleetRollout_OnRegressionRollbackStampsMembers(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	const app = "rollback"
	fleetMember(t, c, ns, "w0", app, "0")
	fleetMember(t, c, ns, "w1", app, "1")

	soak := metav1.Duration{Duration: 30 * time.Minute}
	fr := &stagesv1.FleetRollout{
		ObjectMeta: metav1.ObjectMeta{Name: "fleet-rollback"},
		Spec: stagesv1.FleetRolloutSpec{
			TargetVersion:   "2.0.0",
			PreviousVersion: "1.0.0",
			OnRegression:    "Rollback",
			Selector:        appSelector(app),
			Waves: []stagesv1.FleetWave{
				{Name: "canary", Selector: ringSelector("0"), Soak: &soak},
				{Name: "broad", Selector: ringSelector("1")},
			},
		},
	}
	if err := c.Create(context.Background(), fr); err != nil {
		t.Fatalf("create FleetRollout: %v", err)
	}
	t0 := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)

	reconcileFleetAt(t, c, "fleet-rollback", t0, nil)
	settleMember(t, c, ns, "w0", "2.0.0")
	reconcileFleetAt(t, c, "fleet-rollback", t0, nil) // wave 0 settled + soaking
	unsettleMember(t, c, ns, "w0")
	reconcileFleetAt(t, c, "fleet-rollback", t0.Add(time.Minute), nil) // regression → halt + rollback

	if got := getFleet(t, c, "fleet-rollback"); got.Status.Phase != stagesv1.FleetHalted {
		t.Fatalf("phase = %q, want Halted", got.Status.Phase)
	}
	if got := getStageSet(t, c, ns, "w0").Annotations[rollbackToAnnotation]; got != "1.0.0" {
		t.Fatalf("regressed member should carry rollback-to=1.0.0, got %q", got)
	}
}

// TestFleetRollout_RollbackRequiresPreviousVersion proves the CEL rule rejects a
// Rollback rollout with no previousVersion.
func TestFleetRollout_RollbackRequiresPreviousVersion(t *testing.T) {
	c := testClient(t)
	fr := &stagesv1.FleetRollout{
		ObjectMeta: metav1.ObjectMeta{Name: "fleet-badrollback"},
		Spec: stagesv1.FleetRolloutSpec{
			TargetVersion: "2.0.0",
			OnRegression:  "Rollback", // no previousVersion
			Selector:      appSelector("bad"),
			Waves:         []stagesv1.FleetWave{{Name: "canary", Selector: ringSelector("0")}},
		},
	}
	if err := c.Create(context.Background(), fr); err == nil {
		t.Fatal("a Rollback rollout without previousVersion must be rejected")
	}
}

// TestFleetRollout_NamespaceSelectorBounds proves the namespaceSelector excludes a
// matching StageSet in an out-of-scope namespace — it is not a member and is never
// approved.
func TestFleetRollout_NamespaceSelectorBounds(t *testing.T) {
	c := testClient(t)
	const app = "nssel"
	inScope := labeledNamespace(t, c, map[string]string{"tenant": "true"})
	outScope := labeledNamespace(t, c, map[string]string{"tenant": "false"})
	fleetMember(t, c, inScope, "app", app, "0")
	fleetMember(t, c, outScope, "app", app, "0")

	fr := &stagesv1.FleetRollout{
		ObjectMeta: metav1.ObjectMeta{Name: "fleet-nssel"},
		Spec: stagesv1.FleetRolloutSpec{
			TargetVersion:     "2.0.0",
			Selector:          appSelector(app),
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"tenant": "true"}},
			Waves:             []stagesv1.FleetWave{{Name: "canary", Selector: ringSelector("0")}},
		},
	}
	if err := c.Create(context.Background(), fr); err != nil {
		t.Fatalf("create FleetRollout: %v", err)
	}
	reconcileFleet(t, c, "fleet-nssel")

	if got := memberApproved(t, c, inScope, "app"); got != "2.0.0" {
		t.Fatalf("in-scope member should be approved, got %q", got)
	}
	if got := memberApproved(t, c, outScope, "app"); got != "" {
		t.Fatalf("out-of-scope member must not be approved, got %q", got)
	}
	if fr := getFleet(t, c, "fleet-nssel"); len(fr.Status.Waves) != 1 || fr.Status.Waves[0].Total != 1 {
		t.Fatalf("wave should have exactly 1 member, got %+v", fr.Status.Waves)
	}
}

// TestFleetRollout_ContestedMembersRefused proves a StageSet selected by two
// FleetRollouts fails the rollout closed rather than letting both fight over its
// approval annotation.
func TestFleetRollout_ContestedMembersRefused(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	const app = "contested"
	fleetMember(t, c, ns, "shared", app, "0")

	mkFleet := func(name, target string) *stagesv1.FleetRollout {
		return &stagesv1.FleetRollout{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: stagesv1.FleetRolloutSpec{
				TargetVersion: target,
				Selector:      appSelector(app),
				Waves:         []stagesv1.FleetWave{{Name: "canary", Selector: ringSelector("0")}},
			},
		}
	}
	for _, fr := range []*stagesv1.FleetRollout{mkFleet("contest-a", "2.0.0"), mkFleet("contest-b", "3.0.0")} {
		if err := c.Create(context.Background(), fr); err != nil {
			t.Fatalf("create %s: %v", fr.Name, err)
		}
	}

	reconcileFleet(t, c, "contest-a")
	got := getFleet(t, c, "contest-a")
	cond := apimeta.FindStatusCondition(got.Status.Conditions, ConditionReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != ReasonFleetMembersContested {
		t.Fatalf("Ready condition = %+v, want False/MembersContested", cond)
	}
	if got := memberApproved(t, c, ns, "shared"); got != "" {
		t.Fatalf("a contested member must not be approved, got %q", got)
	}
}

// TestFleetRollout_UnassignedMemberIsFlagged proves a selected StageSet that
// matches no wave fails the rollout closed rather than being silently skipped.
func TestFleetRollout_UnassignedMemberIsFlagged(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	const app = "orphan"
	fleetMember(t, c, ns, "assigned", app, "0")
	fleetMember(t, c, ns, "orphan", app, "") // no ring label

	fr := &stagesv1.FleetRollout{
		ObjectMeta: metav1.ObjectMeta{Name: "fleet-orphan"},
		Spec: stagesv1.FleetRolloutSpec{
			TargetVersion: "2.0.0",
			Selector:      appSelector(app),
			Waves:         []stagesv1.FleetWave{{Name: "canary", Selector: ringSelector("0")}},
		},
	}
	if err := c.Create(context.Background(), fr); err != nil {
		t.Fatalf("create FleetRollout: %v", err)
	}
	reconcileFleet(t, c, "fleet-orphan")

	got := getFleet(t, c, "fleet-orphan")
	cond := apimeta.FindStatusCondition(got.Status.Conditions, ConditionReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != ReasonFleetMembersUnassigned {
		t.Fatalf("Ready condition = %+v, want False/MembersUnassigned", cond)
	}
	if got := memberApproved(t, c, ns, "orphan"); got != "" {
		t.Fatalf("an unassigned member must not be approved, got %q", got)
	}
}
