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

// fleetMember creates a minimal fleet-managed StageSet (approvalMode: Always) with
// the given labels, deployed at 1.0.0. The FleetRollout reconciler only reads its
// labels and status and stamps its approval annotation; the StageSet reconciler is
// not run here.
func fleetMember(t *testing.T, c client.Client, ns, name string, labels map[string]string) {
	t.Helper()
	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: labels},
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
	ss.Status.ObservedGeneration = ss.Generation
	apimeta.SetStatusCondition(&ss.Status.Conditions, metav1.Condition{
		Type: ConditionReady, Status: metav1.ConditionTrue, Reason: ReasonReady,
		Message: "ok", ObservedGeneration: ss.Generation,
	})
	if err := c.Status().Update(context.Background(), ss); err != nil {
		t.Fatalf("settle member %s: %v", name, err)
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

// TestFleetRollout_OpensWavesInOrder drives the whole Phase-1 loop: wave 0 is
// approved first and wave 1 is held; only once wave 0 settles at the target does
// wave 1 open; once it settles the rollout completes.
func TestFleetRollout_OpensWavesInOrder(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)

	fleetMember(t, c, ns, "app0a", map[string]string{"app": "moodle", "ring": "0"})
	fleetMember(t, c, ns, "app0b", map[string]string{"app": "moodle", "ring": "0"})
	fleetMember(t, c, ns, "app1a", map[string]string{"app": "moodle", "ring": "1"})

	fr := &stagesv1.FleetRollout{
		ObjectMeta: metav1.ObjectMeta{Name: "moodle-2"},
		Spec: stagesv1.FleetRolloutSpec{
			TargetVersion: "2.0.0",
			Selector:      metav1.LabelSelector{MatchLabels: map[string]string{"app": "moodle"}},
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
	reconcileFleet(t, c, "moodle-2")
	if got := memberApproved(t, c, ns, "app0a"); got != "2.0.0" {
		t.Fatalf("wave-0 member app0a should be approved to 2.0.0, got %q", got)
	}
	if got := memberApproved(t, c, ns, "app0b"); got != "2.0.0" {
		t.Fatalf("wave-0 member app0b should be approved, got %q", got)
	}
	if got := memberApproved(t, c, ns, "app1a"); got != "" {
		t.Fatalf("wave-1 member app1a must be held until wave 0 settles, got %q", got)
	}
	if fr := getFleet(t, c, "moodle-2"); fr.Status.Phase != stagesv1.FleetInProgress || fr.Status.CurrentWave != "canary" {
		t.Fatalf("phase/wave = %q/%q, want InProgress/canary", fr.Status.Phase, fr.Status.CurrentWave)
	}

	// Wave 0 reaches the target → wave 1 opens.
	settleMember(t, c, ns, "app0a", "2.0.0")
	settleMember(t, c, ns, "app0b", "2.0.0")
	reconcileFleet(t, c, "moodle-2")
	if got := memberApproved(t, c, ns, "app1a"); got != "2.0.0" {
		t.Fatalf("wave-1 member app1a should be approved once wave 0 settled, got %q", got)
	}
	if fr := getFleet(t, c, "moodle-2"); fr.Status.CurrentWave != "broad" {
		t.Fatalf("current wave = %q, want broad", fr.Status.CurrentWave)
	}

	// Wave 1 reaches the target → the rollout completes.
	settleMember(t, c, ns, "app1a", "2.0.0")
	reconcileFleet(t, c, "moodle-2")
	final := getFleet(t, c, "moodle-2")
	if final.Status.Phase != stagesv1.FleetCompleted {
		t.Fatalf("phase = %q, want Completed", final.Status.Phase)
	}
	if cond := apimeta.FindStatusCondition(final.Status.Conditions, ConditionReady); cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != ReasonFleetCompleted {
		t.Fatalf("Ready condition = %+v, want True/Completed", cond)
	}
}

// TestFleetRollout_NamespaceSelectorBounds proves the namespaceSelector excludes a
// matching StageSet in an out-of-scope namespace — it is not a member and is never
// approved.
func TestFleetRollout_NamespaceSelectorBounds(t *testing.T) {
	c := testClient(t)
	inScope := labeledNamespace(t, c, map[string]string{"tenant": "true"})
	outScope := labeledNamespace(t, c, map[string]string{"tenant": "false"})
	fleetMember(t, c, inScope, "app", map[string]string{"app": "moodle", "ring": "0"})
	fleetMember(t, c, outScope, "app", map[string]string{"app": "moodle", "ring": "0"})

	fr := &stagesv1.FleetRollout{
		ObjectMeta: metav1.ObjectMeta{Name: "moodle-ns"},
		Spec: stagesv1.FleetRolloutSpec{
			TargetVersion:     "2.0.0",
			Selector:          metav1.LabelSelector{MatchLabels: map[string]string{"app": "moodle"}},
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"tenant": "true"}},
			Waves:             []stagesv1.FleetWave{{Name: "canary", Selector: ringSelector("0")}},
		},
	}
	if err := c.Create(context.Background(), fr); err != nil {
		t.Fatalf("create FleetRollout: %v", err)
	}
	reconcileFleet(t, c, "moodle-ns")

	if got := memberApproved(t, c, inScope, "app"); got != "2.0.0" {
		t.Fatalf("in-scope member should be approved, got %q", got)
	}
	if got := memberApproved(t, c, outScope, "app"); got != "" {
		t.Fatalf("out-of-scope member must not be approved, got %q", got)
	}
	// Only the in-scope member counts toward the wave.
	if fr := getFleet(t, c, "moodle-ns"); len(fr.Status.Waves) != 1 || fr.Status.Waves[0].Total != 1 {
		t.Fatalf("wave should have exactly 1 member, got %+v", fr.Status.Waves)
	}
}

// TestFleetRollout_UnassignedMemberIsFlagged proves a selected StageSet that
// matches no wave fails the rollout closed rather than being silently skipped.
func TestFleetRollout_UnassignedMemberIsFlagged(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	fleetMember(t, c, ns, "assigned", map[string]string{"app": "moodle", "ring": "0"})
	fleetMember(t, c, ns, "orphan", map[string]string{"app": "moodle"}) // no ring label

	fr := &stagesv1.FleetRollout{
		ObjectMeta: metav1.ObjectMeta{Name: "moodle-orphan"},
		Spec: stagesv1.FleetRolloutSpec{
			TargetVersion: "2.0.0",
			Selector:      metav1.LabelSelector{MatchLabels: map[string]string{"app": "moodle"}},
			Waves:         []stagesv1.FleetWave{{Name: "canary", Selector: ringSelector("0")}},
		},
	}
	if err := c.Create(context.Background(), fr); err != nil {
		t.Fatalf("create FleetRollout: %v", err)
	}
	reconcileFleet(t, c, "moodle-orphan")

	got := getFleet(t, c, "moodle-orphan")
	cond := apimeta.FindStatusCondition(got.Status.Conditions, ConditionReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != ReasonFleetMembersUnassigned {
		t.Fatalf("Ready condition = %+v, want False/MembersUnassigned", cond)
	}
	// The orphan must not have been approved.
	if got := memberApproved(t, c, ns, "orphan"); got != "" {
		t.Fatalf("an unassigned member must not be approved, got %q", got)
	}
}
