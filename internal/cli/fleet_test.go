// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

// fleetViewMember creates a member StageSet and stamps its deployed version and
// Ready status, so the fleet view can render it.
func fleetViewMember(t testing.TB, c client.Client, ns, name, app, ring, version string, ready bool) {
	t.Helper()
	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: map[string]string{"app": app, "ring": ring}},
		Spec: stagesv1.StageSetSpec{
			Interval: metav1.Duration{Duration: time.Minute},
			Version:  &stagesv1.VersionSource{Value: version, ApprovalMode: stagesv1.ApprovalAlways},
			Stages:   []stagesv1.Stage{{Name: "app", SourceRef: stagesv1.SourceReference{Name: "ea"}}},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create member %s: %v", name, err)
	}
	ss.Status.Version = version
	ss.Status.ObservedGeneration = ss.Generation
	status := metav1.ConditionFalse
	if ready {
		status = metav1.ConditionTrue
	}
	apimeta.SetStatusCondition(&ss.Status.Conditions, metav1.Condition{
		Type: "Ready", Status: status, Reason: "Succeeded", Message: "ok", ObservedGeneration: ss.Generation,
	})
	if err := c.Status().Update(context.Background(), ss); err != nil {
		t.Fatalf("set member %s status: %v", name, err)
	}
}

func TestFleet_ShowsWaveProgress(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "fleetview")
	const app = "view"
	fleetViewMember(t, c, ns, "w0", app, "0", "2.0.0", true) // at target, Ready
	fleetViewMember(t, c, ns, "w1", app, "1", "1.0.0", true) // still on old version, held

	fr := &stagesv1.FleetRollout{
		ObjectMeta: metav1.ObjectMeta{Name: "view-fleet"},
		Spec: stagesv1.FleetRolloutSpec{
			TargetVersion: "2.0.0",
			Selector:      metav1.LabelSelector{MatchLabels: map[string]string{"app": app}},
			Waves: []stagesv1.FleetWave{
				{Name: "canary", Selector: metav1.LabelSelector{MatchLabels: map[string]string{"ring": "0"}}},
				{Name: "broad", Selector: metav1.LabelSelector{MatchLabels: map[string]string{"ring": "1"}}},
			},
		},
	}
	if err := c.Create(context.Background(), fr); err != nil {
		t.Fatalf("create FleetRollout: %v", err)
	}
	fr.Status.Phase = stagesv1.FleetInProgress
	fr.Status.CurrentWave = "broad"
	fr.Status.Waves = []stagesv1.FleetWaveStatus{
		{Name: "canary", Total: 1, AtTarget: 1, Ready: 1, Settled: true},
		{Name: "broad", Total: 1, AtTarget: 0, Ready: 1},
	}
	if err := c.Status().Update(context.Background(), fr); err != nil {
		t.Fatalf("set fleet status: %v", err)
	}

	stdout, stderr, code := runCLI(t, cfg, "fleet", "view-fleet")
	if code != exitOK {
		t.Fatalf("fleet view exit = %d (stderr=%s)\n%s", code, stderr, stdout)
	}
	for _, want := range []string{
		"FleetRollout view-fleet", "version 2.0.0", "phase: InProgress", "wave: broad",
		"wave canary", "1/1 at 2.0.0", "settled", "✓", "w0",
		"wave broad", "w1", "held → awaiting 2.0.0",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("fleet view missing %q:\n%s", want, stdout)
		}
	}
}

func TestFleet_JSONOutput(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	fr := &stagesv1.FleetRollout{
		ObjectMeta: metav1.ObjectMeta{Name: "view-json"},
		Spec: stagesv1.FleetRolloutSpec{
			TargetVersion: "2.0.0",
			Selector:      metav1.LabelSelector{MatchLabels: map[string]string{"app": "json"}},
			Waves:         []stagesv1.FleetWave{{Name: "canary", Selector: metav1.LabelSelector{MatchLabels: map[string]string{"ring": "0"}}}},
		},
	}
	if err := c.Create(context.Background(), fr); err != nil {
		t.Fatalf("create FleetRollout: %v", err)
	}
	stdout, stderr, code := runCLI(t, cfg, "fleet", "view-json", "-o", "json")
	if code != exitOK {
		t.Fatalf("fleet -o json exit = %d (stderr=%s)", code, stderr)
	}
	if !strings.Contains(stdout, "\"kind\": \"FleetRollout\"") || !strings.Contains(stdout, "\"targetVersion\": \"2.0.0\"") {
		t.Errorf("fleet -o json missing expected fields:\n%s", stdout)
	}
}
