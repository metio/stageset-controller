// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

// scopedLadderStageSet builds a StageSet whose post-ladder holds one
// Revision-scoped and one Version-scoped HTTP action, each hitting its own
// counter, at an inline pinned version.
func scopedLadderStageSet(ns, name, version, revEndpoint, verEndpoint string) *stagesv1.StageSet {
	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: stagesv1.StageSetSpec{
			Interval: metav1.Duration{Duration: time.Minute},
			Stages: []stagesv1.Stage{{
				Name:      "app",
				SourceRef: stagesv1.SourceReference{Name: "ea"},
				Actions: &stagesv1.StageActions{Post: []stagesv1.Action{
					{Name: "rev-notify", Scope: stagesv1.ScopeRevision, HTTP: &stagesv1.HTTPAction{URL: revEndpoint}},
					{Name: "ver-notify", Scope: stagesv1.ScopeVersion, HTTP: &stagesv1.HTTPAction{URL: verEndpoint}},
				}},
			}},
		},
	}
	if version != "" {
		ss.Spec.Version = &stagesv1.VersionSource{Value: version}
	}
	return ss
}

// The flagship guarantee: at a fixed version, churning the revision (a
// config-only change) re-runs a Revision-scoped action but never a
// Version-scoped one. This is the wart the feature exists to fix — today's
// per-revision ledger re-fires the whole upgrade ladder on any ConfigMap edit.
func TestActionScope_VersionSurvivesRevisionChurn(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	var revHits, verHits int32
	revEndpoint := countingServer(t, http.StatusOK, &revHits)
	verEndpoint := countingServer(t, http.StatusOK, &verHits)
	hosts := []string{actionHost(t, revEndpoint), actionHost(t, verEndpoint)}

	servedArtifact(t, c, ns, "ea", "", map[string]string{"cm.yaml": configMapManifest(ns, "obj")})
	ss := scopedLadderStageSet(ns, "churn", "1.0.0", revEndpoint, verEndpoint)
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}

	// Reconcile 1 is first adoption: it baselines the version (records 1.0.0,
	// runs no version-scoped work) while the Revision action runs normally.
	if err := reconcileWith(t, c, ss, hosts); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	// A config-only change: new artifact revision, unchanged version.
	repointArtifact(t, c, ns, "ea", map[string]string{"cm.yaml": configMapManifest(ns, "obj-v2")})
	if err := reconcileWith(t, c, ss, hosts); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}

	if n := atomic.LoadInt32(&revHits); n != 2 {
		t.Errorf("Revision-scoped action fired %d times; a new revision must re-run it (want 2)", n)
	}
	if n := atomic.LoadInt32(&verHits); n != 0 {
		t.Errorf("Version-scoped action fired %d times; revision churn at a fixed version must never run it (want 0)", n)
	}
}

// A Version-scoped action fires when the resolved version changes — the new
// episode — after having been baselined on adoption.
func TestActionScope_VersionFiresOnVersionChange(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	var revHits, verHits int32
	revEndpoint := countingServer(t, http.StatusOK, &revHits)
	verEndpoint := countingServer(t, http.StatusOK, &verHits)
	hosts := []string{actionHost(t, revEndpoint), actionHost(t, verEndpoint)}

	servedArtifact(t, c, ns, "ea", "", map[string]string{"cm.yaml": configMapManifest(ns, "obj")})
	ss := scopedLadderStageSet(ns, "bump", "1.0.0", revEndpoint, verEndpoint)
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	if err := reconcileWith(t, c, ss, hosts); err != nil {
		t.Fatalf("reconcile 1 (baseline at 1.0.0): %v", err)
	}
	if n := atomic.LoadInt32(&verHits); n != 0 {
		t.Fatalf("Version action ran on adoption baseline (want 0), fired %d", n)
	}

	// Cross a version boundary: a new episode runs the version ladder.
	live := getStageSet(t, c, ns, "bump")
	live.Spec.Version.Value = "2.0.0"
	if err := c.Update(context.Background(), live); err != nil {
		t.Fatalf("bump version: %v", err)
	}
	if err := reconcileWith(t, c, live, hosts); err != nil {
		t.Fatalf("reconcile 2 (version 2.0.0): %v", err)
	}
	if n := atomic.LoadInt32(&verHits); n != 1 {
		t.Errorf("Version action fired %d times after a version change; want 1 (the new episode)", n)
	}
}

// --reset-scope=Version clears the version ledger, re-running a Version-scoped
// action once at the unchanged version — the deliberate "re-run the upgrade"
// escape hatch. It is one-shot: the reset fires only for a fresh token.
func TestActionScope_ResetScopeRerunsAtSameVersion(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	var revHits, verHits int32
	revEndpoint := countingServer(t, http.StatusOK, &revHits)
	verEndpoint := countingServer(t, http.StatusOK, &verHits)
	hosts := []string{actionHost(t, revEndpoint), actionHost(t, verEndpoint)}

	servedArtifact(t, c, ns, "ea", "", map[string]string{"cm.yaml": configMapManifest(ns, "obj")})
	ss := scopedLadderStageSet(ns, "reset", "1.0.0", revEndpoint, verEndpoint)
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	// Adoption baselines at 1.0.0; bump to 2.0.0 so the version ledger holds a
	// genuinely-run entry to reset.
	if err := reconcileWith(t, c, ss, hosts); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	live := getStageSet(t, c, ns, "reset")
	live.Spec.Version.Value = "2.0.0"
	if err := c.Update(context.Background(), live); err != nil {
		t.Fatalf("bump version: %v", err)
	}
	if err := reconcileWith(t, c, live, hosts); err != nil {
		t.Fatalf("reconcile 2 (2.0.0 runs ver): %v", err)
	}
	if n := atomic.LoadInt32(&verHits); n != 1 {
		t.Fatalf("ver action should have run once at 2.0.0, ran %d", n)
	}

	// Stamp a reset token: the version ledger clears and ver re-runs at 2.0.0.
	stampAnnotation(t, c, ns, "reset", "stages.metio.wtf/reset-scope", "tok-1")
	live = getStageSet(t, c, ns, "reset")
	if err := reconcileWith(t, c, live, hosts); err != nil {
		t.Fatalf("reconcile 3 (reset): %v", err)
	}
	if n := atomic.LoadInt32(&verHits); n != 2 {
		t.Errorf("--reset-scope must re-run the Version action at the same version; ran %d, want 2", n)
	}

	// One-shot: reconciling again with no fresh token must not re-run it.
	live = getStageSet(t, c, ns, "reset")
	if live.Status.LastHandledResetScope != "tok-1" {
		t.Errorf("status.lastHandledResetScope = %q, want tok-1", live.Status.LastHandledResetScope)
	}
	if err := reconcileWith(t, c, live, hosts); err != nil {
		t.Fatalf("reconcile 4: %v", err)
	}
	if n := atomic.LoadInt32(&verHits); n != 2 {
		t.Errorf("reset must be one-shot; ver ran %d after a second reconcile, want 2", n)
	}
}

// stampAnnotation sets a single annotation on a StageSet and persists it.
func stampAnnotation(t *testing.T, c client.Client, ns, name, key, value string) {
	t.Helper()
	var ss stagesv1.StageSet
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &ss); err != nil {
		t.Fatalf("get %s/%s: %v", ns, name, err)
	}
	ann := ss.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	ann[key] = value
	ss.SetAnnotations(ann)
	if err := c.Update(context.Background(), &ss); err != nil {
		t.Fatalf("stamp annotation: %v", err)
	}
}

// First adoption of a versioned StageSet baselines its Version-scoped actions:
// they are recorded complete without running, so migrating a running fleet in
// does not trigger its maintenance choreography. status.version records the
// adopted value.
func TestActionScope_AdoptionBaselinesVersionActions(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	var revHits, verHits int32
	revEndpoint := countingServer(t, http.StatusOK, &revHits)
	verEndpoint := countingServer(t, http.StatusOK, &verHits)
	hosts := []string{actionHost(t, revEndpoint), actionHost(t, verEndpoint)}

	servedArtifact(t, c, ns, "ea", "", map[string]string{"cm.yaml": configMapManifest(ns, "obj")})
	ss := scopedLadderStageSet(ns, "adopt", "3.1.4", revEndpoint, verEndpoint)
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	if err := reconcileWith(t, c, ss, hosts); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if n := atomic.LoadInt32(&verHits); n != 0 {
		t.Errorf("Version action fired %d times on first adoption; baseline must record without running (want 0)", n)
	}
	if n := atomic.LoadInt32(&revHits); n != 1 {
		t.Errorf("Revision action fired %d times; adoption does not baseline it (want 1)", n)
	}
	got := getStageSet(t, c, ns, "adopt")
	if got.Status.Version != "3.1.4" {
		t.Errorf("status.version = %q, want the baselined 3.1.4", got.Status.Version)
	}
	var appVer *stagesv1.StageStatus
	for i := range got.Status.Stages {
		if got.Status.Stages[i].Name == "app" {
			appVer = &got.Status.Stages[i]
		}
	}
	if appVer == nil {
		t.Fatal("no status for stage app")
	}
	if appVer.LedgerVersion != "3.1.4" {
		t.Errorf("stage ledgerVersion = %q, want 3.1.4", appVer.LedgerVersion)
	}
	if len(appVer.ExecutedVersionActions) != 1 || appVer.ExecutedVersionActions[0] != "ver-notify" {
		t.Errorf("version ledger = %v, want [ver-notify] recorded by the baseline", appVer.ExecutedVersionActions)
	}
}
