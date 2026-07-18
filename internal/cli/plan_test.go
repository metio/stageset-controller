// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/fluxcd/pkg/apis/meta"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/inventory"
	"github.com/metio/stageset-controller/internal/stageinv"
)

// planStageSet builds a one-stage StageSet with a Revision pre action and a
// Lifetime post action, so a plan exercises both scopes.
func planStageSet(t testing.TB, c client.Client, ns, name string) {
	t.Helper()
	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: stagesv1.StageSetSpec{
			Interval: metav1.Duration{Duration: 5 * time.Minute},
			Stages: []stagesv1.Stage{{
				Name:      "first",
				SourceRef: stagesv1.SourceReference{Name: name + "-ea"},
				Actions: &stagesv1.StageActions{
					Pre:  []stagesv1.Action{{Name: "check", Scope: stagesv1.ScopeRevision, Wait: &stagesv1.WaitAction{Expr: "true"}}},
					Post: []stagesv1.Action{{Name: "install-database", Scope: stagesv1.ScopeLifetime, Wait: &stagesv1.WaitAction{Expr: "true"}}},
				},
			}},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
}

func TestPlan_ShowsActionVerdicts(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "plan")
	planStageSet(t, c, ns, "app")
	// install-database is recorded complete (unanchored), so it must SKIP.
	makeLedgerWithCompletion(t, c, ns, "app", "first", "install-database", stagesv1.OriginExecuted)
	dir := writeSourceTree(t, map[string]string{"cm.yaml": configMapManifest(ns, "settings", nil)})

	stdout, stderr, code := runCLI(t, cfg, "plan", "app", "-n", ns, "--source-dir", dir)
	// A Revision action would run, so the plan is non-empty: diff-style exit 1.
	if code != exitDiff {
		t.Fatalf("plan exit = %d (stderr=%s)\n%s", code, stderr, stdout)
	}
	for _, want := range []string{"check", "WILL RUN", "install-database", "SKIP", "Lifetime"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("plan output missing %q:\n%s", want, stdout)
		}
	}
}

func TestPlan_ExitZeroWhenNothingRuns(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "planzero")
	// A stage whose only action is a completed Lifetime one: nothing would run.
	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "app"},
		Spec: stagesv1.StageSetSpec{
			Interval: metav1.Duration{Duration: 5 * time.Minute},
			Stages: []stagesv1.Stage{{
				Name:      "first",
				SourceRef: stagesv1.SourceReference{Name: "app-ea"},
				Actions: &stagesv1.StageActions{
					Post: []stagesv1.Action{{Name: "install-database", Scope: stagesv1.ScopeLifetime, Wait: &stagesv1.WaitAction{Expr: "true"}}},
				},
			}},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	makeLedgerWithCompletion(t, c, ns, "app", "first", "install-database", stagesv1.OriginExecuted)
	dir := writeSourceTree(t, map[string]string{"cm.yaml": configMapManifest(ns, "settings", nil)})

	if _, stderr, code := runCLI(t, cfg, "plan", "app", "-n", ns, "--source-dir", dir); code != exitOK {
		t.Fatalf("plan with nothing to run should exit 0, got %d (stderr=%s)", code, stderr)
	}
}

func containsSubstr(lines []string, sub string) bool {
	for _, l := range lines {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

func TestPlanGates(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

	// A Deny update window covering `now`: delivery is held.
	deny := &stagesv1.StageSet{Spec: stagesv1.StageSetSpec{
		UpdateWindows: []stagesv1.UpdateWindow{{
			Type: "Deny",
			From: &metav1.Time{Time: now.Add(-time.Hour)},
			To:   &metav1.Time{Time: now.Add(time.Hour)},
		}},
	}}
	if g := planGates(deny, nil, deny.Spec.Stages, now); !containsSubstr(g, "update window") {
		t.Errorf("a covering Deny window should HOLD: %v", g)
	}

	// A status error-budget freeze.
	budget := &stagesv1.StageSet{Status: stagesv1.StageSetStatus{
		BudgetFreeze: &stagesv1.BudgetFreeze{Remaining: "0", ResumeThreshold: "0.05"},
	}}
	if g := planGates(budget, nil, nil, now); !containsSubstr(g, "error budget") {
		t.Errorf("a status budget freeze should HOLD: %v", g)
	}

	// A stage awaiting a manual promotion.
	promo := &stagesv1.StageSet{Spec: stagesv1.StageSetSpec{Stages: []stagesv1.Stage{{Name: "app"}}}}
	prior := map[string]stagesv1.StageStatus{"app": {PromotionState: &stagesv1.PromotionState{Phase: stagesv1.PromotionAwaitingManual}}}
	if g := planGates(promo, prior, promo.Spec.Stages, now); !containsSubstr(g, "promotion (app)") {
		t.Errorf("a stage awaiting promotion should HOLD: %v", g)
	}

	// Nothing holding: no gate lines.
	if g := planGates(&stagesv1.StageSet{}, nil, nil, now); len(g) != 0 {
		t.Errorf("no gates expected, got %v", g)
	}
}

func TestPlan_ShowsPendingMigrations(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "planmig")
	planStageSet(t, c, ns, "app")
	// The controller's last reconcile queued a migration.
	ss := getStageSetCLI(t, c, ns, "app")
	ss.Status.PendingMigrations = []stagesv1.PendingMigration{{
		Name: "schema-1-1", To: "1.1.0", From: "1.0.x", Stage: "first", Actions: []string{"job"},
	}}
	if err := c.Status().Update(context.Background(), ss); err != nil {
		t.Fatalf("set pending migrations: %v", err)
	}
	dir := writeSourceTree(t, map[string]string{"cm.yaml": configMapManifest(ns, "settings", nil)})

	stdout, stderr, code := runCLI(t, cfg, "plan", "app", "-n", ns, "--source-dir", dir)
	if code != exitDiff {
		t.Fatalf("a pending migration makes the plan non-empty (exit 1); got %d (stderr=%s)\n%s", code, stderr, stdout)
	}
	for _, want := range []string{"migrations:", "schema-1-1", "before first"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("plan output missing %q:\n%s", want, stdout)
		}
	}
}

func getStageSetCLI(t testing.TB, c client.Client, ns, name string) *stagesv1.StageSet {
	t.Helper()
	var ss stagesv1.StageSet
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &ss); err != nil {
		t.Fatalf("get StageSet: %v", err)
	}
	return &ss
}

func TestIsStateBearing(t *testing.T) {
	tests := []struct {
		ref  inventory.ObjectRef
		want bool
	}{
		{inventory.ObjectRef{Kind: "PersistentVolumeClaim"}, true},
		{inventory.ObjectRef{Kind: "PersistentVolume"}, true},
		{inventory.ObjectRef{Group: "apps", Kind: "StatefulSet"}, true},
		{inventory.ObjectRef{Kind: "ConfigMap"}, false},
		{inventory.ObjectRef{Group: "apps", Kind: "Deployment"}, false},
	}
	for _, tc := range tests {
		if got := isStateBearing(tc.ref); got != tc.want {
			t.Errorf("isStateBearing(%s/%s) = %v, want %v", tc.ref.Group, tc.ref.Kind, got, tc.want)
		}
	}
}

func TestPlan_ResolvesVersionFromArtifact(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "planver")
	// A StageSet whose version is read from a file in the stage's source tree.
	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "app"},
		Spec: stagesv1.StageSetSpec{
			Interval: metav1.Duration{Duration: 5 * time.Minute},
			Version:  &stagesv1.VersionSource{FromArtifact: &stagesv1.ArtifactVersionRef{Stage: "first", Path: "VERSION"}},
			Stages: []stagesv1.Stage{{
				Name:      "first",
				SourceRef: stagesv1.SourceReference{Name: "app-ea"},
				Actions: &stagesv1.StageActions{
					Pre: []stagesv1.Action{{Name: "migrate", Scope: stagesv1.ScopeVersion, Wait: &stagesv1.WaitAction{Expr: "true"}}},
				},
			}},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	// A prior version makes the render a real version transition, so the
	// Version-scoped action runs rather than baselining on adoption.
	created := getStageSetCLI(t, c, ns, "app")
	created.Status.Version = "2.3.0"
	if err := c.Status().Update(context.Background(), created); err != nil {
		t.Fatalf("set status.version: %v", err)
	}
	dir := writeSourceTree(t, map[string]string{
		"cm.yaml": configMapManifest(ns, "settings", nil),
		"VERSION": "2.3.4\n",
	})

	stdout, stderr, code := runCLI(t, cfg, "plan", "app", "-n", ns, "--source-dir", dir)
	if code != exitDiff {
		t.Fatalf("plan exit = %d (stderr=%s)\n%s", code, stderr, stdout)
	}
	if !strings.Contains(stdout, "2.3.4") {
		t.Errorf("version should resolve to 2.3.4 from the artifact file:\n%s", stdout)
	}
	if strings.Contains(stdout, "fromArtifact is not resolved") {
		t.Errorf("fromArtifact should now be resolved, not caveated:\n%s", stdout)
	}
	if !strings.Contains(stdout, "migrate") || !strings.Contains(stdout, "WILL RUN") {
		t.Errorf("the version-transition action should run:\n%s", stdout)
	}
}

func TestPlan_JSONOutput(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "planjson")
	planStageSet(t, c, ns, "app")
	makeLedgerWithCompletion(t, c, ns, "app", "first", "install-database", stagesv1.OriginExecuted)
	dir := writeSourceTree(t, map[string]string{"cm.yaml": configMapManifest(ns, "settings", nil)})

	stdout, stderr, code := runCLI(t, cfg, "plan", "app", "-n", ns, "--source-dir", dir, "-o", "json")
	if code != exitDiff {
		t.Fatalf("plan -o json exit = %d (stderr=%s)", code, stderr)
	}
	var plans []stageSetPlan
	if err := json.Unmarshal([]byte(stdout), &plans); err != nil {
		t.Fatalf("plan -o json is not valid JSON: %v\n%s", err, stdout)
	}
	if len(plans) != 1 || plans[0].Name != "app" || plans[0].Namespace != ns {
		t.Fatalf("expected one plan for %s/app, got %+v", ns, plans)
	}
	if !plans[0].WouldRun {
		t.Errorf("app plan should report wouldRun")
	}
	states := map[string]string{}
	for _, s := range plans[0].Stages {
		for _, a := range s.Actions {
			states[a.Name] = a.State
		}
	}
	if states["check"] != "WILL RUN" {
		t.Errorf("check should be WILL RUN, got %q", states["check"])
	}
	if states["install-database"] != "SKIP" {
		t.Errorf("completed Lifetime action should be SKIP, got %q", states["install-database"])
	}
}

func TestPlan_FanOutAllNamespaces(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	nsA := makeNamespace(t, c, "planfanoa")
	nsB := makeNamespace(t, c, "planfanob")
	planStageSet(t, c, nsA, "app")
	planStageSet(t, c, nsB, "web")
	// The shared envtest apiserver holds StageSets from other cases too, so this
	// cross-namespace fan-out is scoped to a label only these two carry.
	labelStageSet(t, c, nsA, "app", map[string]string{"plan-fanout": "allns"})
	labelStageSet(t, c, nsB, "web", map[string]string{"plan-fanout": "allns"})
	dir := writeSourceTree(t, map[string]string{"cm.yaml": configMapManifest(nsA, "settings", nil)})

	stdout, stderr, code := runCLI(t, cfg, "plan", "-A", "-l", "plan-fanout=allns", "-o", "json", "--source-dir", dir)
	if code != exitDiff {
		t.Fatalf("fan-out plan exit = %d (stderr=%s)\n%s", code, stderr, stdout)
	}
	var plans []stageSetPlan
	if err := json.Unmarshal([]byte(stdout), &plans); err != nil {
		t.Fatalf("fan-out is not valid JSON: %v\n%s", err, stdout)
	}
	names := map[string]bool{}
	for _, p := range plans {
		names[p.Namespace+"/"+p.Name] = true
	}
	if len(plans) != 2 || !names[nsA+"/app"] || !names[nsB+"/web"] {
		t.Fatalf("--all-namespaces should plan exactly both labeled StageSets, saw %v", names)
	}
}

func labelStageSet(t testing.TB, c client.Client, ns, name string, labels map[string]string) {
	t.Helper()
	ss := getStageSetCLI(t, c, ns, name)
	ss.Labels = labels
	if err := c.Update(context.Background(), ss); err != nil {
		t.Fatalf("label StageSet %s/%s: %v", ns, name, err)
	}
}

func TestPlan_FanOutRecordsBrokenStageSet(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "planbroken")
	planStageSet(t, c, ns, "good")
	// A StageSet whose decryption key Secret does not exist cannot be planned.
	broken := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "broken"},
		Spec: stagesv1.StageSetSpec{
			Interval:   metav1.Duration{Duration: 5 * time.Minute},
			Decryption: &stagesv1.Decryption{Provider: "sops", SecretRef: &meta.LocalObjectReference{Name: "missing-keys"}},
			Stages:     []stagesv1.Stage{{Name: "first", SourceRef: stagesv1.SourceReference{Name: "broken-ea"}}},
		},
	}
	if err := c.Create(context.Background(), broken); err != nil {
		t.Fatalf("create broken StageSet: %v", err)
	}
	dir := writeSourceTree(t, map[string]string{"cm.yaml": configMapManifest(ns, "settings", nil)})

	stdout, _, code := runCLI(t, cfg, "plan", "-n", ns, "-o", "json", "--source-dir", dir)
	// A StageSet that cannot be planned makes the run exit 3, but the healthy one
	// is still planned — a broken tenant must not hide the rest of the fleet.
	if code != exitError {
		t.Fatalf("a broken StageSet should exit 3, got %d\n%s", code, stdout)
	}
	var plans []stageSetPlan
	if err := json.Unmarshal([]byte(stdout), &plans); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, stdout)
	}
	byName := map[string]stageSetPlan{}
	for _, p := range plans {
		byName[p.Name] = p
	}
	if byName["broken"].Error == "" {
		t.Errorf("the broken StageSet should carry an error, got %+v", byName["broken"])
	}
	if byName["good"].Error != "" {
		t.Errorf("the healthy StageSet should still plan, got error %q", byName["good"].Error)
	}
}

func TestPlan_SelectorFilters(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "plansel")
	planStageSet(t, c, ns, "wanted")
	planStageSet(t, c, ns, "other")
	// Label only one StageSet so the selector picks it out.
	wanted := getStageSetCLI(t, c, ns, "wanted")
	wanted.Labels = map[string]string{"tier": "gold"}
	if err := c.Update(context.Background(), wanted); err != nil {
		t.Fatalf("label StageSet: %v", err)
	}
	dir := writeSourceTree(t, map[string]string{"cm.yaml": configMapManifest(ns, "settings", nil)})

	stdout, _, _ := runCLI(t, cfg, "plan", "-n", ns, "-l", "tier=gold", "-o", "json", "--source-dir", dir)
	var plans []stageSetPlan
	if err := json.Unmarshal([]byte(stdout), &plans); err != nil {
		t.Fatalf("selector plan is not valid JSON: %v\n%s", err, stdout)
	}
	if len(plans) != 1 || plans[0].Name != "wanted" {
		t.Fatalf("selector should match only the labeled StageSet, got %+v", plans)
	}
}

func TestPlan_NameWithFanOutIsUsageError(t *testing.T) {
	cfg := envtestConfig(t)
	if _, _, code := runCLI(t, cfg, "plan", "app", "-A"); code != exitUsage {
		t.Fatalf("name + --all-namespaces should be a usage error (exit 2), got %d", code)
	}
}

func TestPlan_FlagsStateBearingPrune(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "planprune")
	planStageSet(t, c, ns, "app")
	ss := getStageSetCLI(t, c, ns, "app")

	// The stage previously applied a PVC (still live, carrying this StageSet's
	// owner labels) and recorded it in its inventory; the current render no longer
	// contains it, so the next reconcile would prune it.
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: "app-db",
			Labels: map[string]string{"stages.metio.wtf/name": "app", "stages.metio.wtf/namespace": ns},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}},
		},
	}
	if err := c.Create(context.Background(), pvc); err != nil {
		t.Fatalf("create PVC: %v", err)
	}
	rec := &stageinv.Recorder{Client: c}
	if err := rec.Write(context.Background(), ss, "first", 0, []inventory.ObjectRef{
		{Version: "v1", Kind: "PersistentVolumeClaim", Namespace: ns, Name: "app-db"},
	}); err != nil {
		t.Fatalf("record inventory: %v", err)
	}
	dir := writeSourceTree(t, map[string]string{"cm.yaml": configMapManifest(ns, "settings", nil)})

	stdout, stderr, code := runCLI(t, cfg, "plan", "app", "-n", ns, "--source-dir", dir)
	if code != exitDiff {
		t.Fatalf("a pending prune makes the plan non-empty (exit 1); got %d (stderr=%s)\n%s", code, stderr, stdout)
	}
	for _, want := range []string{"prunes:", "⚠", "PersistentVolumeClaim/app-db", "destroys its data"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("plan output missing %q:\n%s", want, stdout)
		}
	}
}
