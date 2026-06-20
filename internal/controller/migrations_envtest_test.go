// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

// deleteLadder is a one-migration ladder (to 2.0.0) whose single action deletes
// the named ConfigMap — an observable side effect that proves the sourced ladder
// was fetched, parsed, anchored, and executed.
func deleteLadder(ns, target string) string {
	return "" +
		"- name: drop-legacy\n" +
		"  to: \"2.0.0\"\n" +
		"  stage: stage-a\n" +
		"  actions:\n" +
		"    - name: delete-legacy\n" +
		"      delete:\n" +
		"        target:\n" +
		"          apiVersion: v1\n" +
		"          kind: ConfigMap\n" +
		"          name: " + target + "\n" +
		"          namespace: " + ns + "\n"
}

// TestReconcile_SourcedMigration_RunsAndAdvancesVersion drives the whole sourced
// path against a real apiserver: resolve the migrationsSourceRef ExternalArtifact,
// fetch + parse the ladder, anchor it to stage-a, run its delete action, record
// the ledger, and advance status.version. Baselining first proves migrations do
// not run on first adoption.
func TestReconcile_SourcedMigration_RunsAndAdvancesVersion(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)

	// The victim the sourced migration will delete.
	victim := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "legacy"}}
	if err := c.Create(context.Background(), victim); err != nil {
		t.Fatalf("create victim: %v", err)
	}
	// Stage artifact so stage-a applies and the run can complete.
	servedArtifact(t, c, ns, "stage-ea", "", map[string]string{"cm.yaml": configMapManifest(ns, "stage-obj")})
	// Migration ladder artifact.
	servedArtifact(t, c, ns, "ladder-ea", "", map[string]string{"ladder.yaml": deleteLadder(ns, "legacy")})

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "migrator"},
		Spec: stagesv1.StageSetSpec{
			Interval:            metav1.Duration{Duration: time.Minute},
			Version:             &stagesv1.VersionSource{Value: "1.0.0"},
			MigrationsSourceRef: &stagesv1.MigrationsSource{SourceRef: stagesv1.SourceReference{Name: "ladder-ea"}},
			Stages:              []stagesv1.Stage{{Name: "stage-a", SourceRef: stagesv1.SourceReference{Name: "stage-ea"}}},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}

	// Baseline at 1.0.0: records the version, runs no migration — victim survives.
	if err := reconcileWith(t, c, ss, nil); err != nil {
		t.Fatalf("baseline reconcile: %v", err)
	}
	base := getStageSet(t, c, ns, "migrator")
	if base.Status.Version != "1.0.0" {
		t.Fatalf("baseline version = %q, want 1.0.0", base.Status.Version)
	}
	if !cmExists(t, c, ns, "legacy") {
		t.Fatal("baseline must not run migrations; legacy should still exist")
	}

	// Advance desired version to 2.0.0 → the transition crosses drop-legacy.
	base.Spec.Version = &stagesv1.VersionSource{Value: "2.0.0"}
	if err := c.Update(context.Background(), base); err != nil {
		t.Fatalf("bump version: %v", err)
	}
	if err := reconcileWith(t, c, base, nil); err != nil {
		t.Fatalf("transition reconcile: %v", err)
	}

	final := getStageSet(t, c, ns, "migrator")
	if final.Status.Version != "2.0.0" {
		t.Fatalf("version did not advance: %q", final.Status.Version)
	}
	if cmExists(t, c, ns, "legacy") {
		t.Fatal("the sourced migration's delete action did not run (legacy still exists)")
	}
	if r := readyReason(final); r != ReasonReady {
		t.Fatalf("Ready reason = %q, want %q", r, ReasonReady)
	}
	// The in-flight ledger is cleared once the transition completes; the deleted
	// victim and the advanced version are the proof the migration ran.
	if len(final.Status.ExecutedMigrations) != 0 {
		t.Fatalf("ledger should be cleared after the transition, got %v", final.Status.ExecutedMigrations)
	}
}

// TestReconcile_SourcedMigration_InvalidArtifactFailsClosed proves a malformed
// sourced ladder fails closed: MigrationArtifactInvalid, version not advanced.
func TestReconcile_SourcedMigration_InvalidArtifactFailsClosed(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	servedArtifact(t, c, ns, "stage-ea", "", map[string]string{"cm.yaml": configMapManifest(ns, "stage-obj")})
	// A migration with an unknown field — strict parsing rejects it.
	servedArtifact(t, c, ns, "ladder-ea", "", map[string]string{
		"ladder.yaml": "- name: x\n  to: \"2.0.0\"\n  stage: stage-a\n  bogusField: true\n",
	})

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "broken"},
		Spec: stagesv1.StageSetSpec{
			Interval:            metav1.Duration{Duration: time.Minute},
			Version:             &stagesv1.VersionSource{Value: "1.0.0"},
			MigrationsSourceRef: &stagesv1.MigrationsSource{SourceRef: stagesv1.SourceReference{Name: "ladder-ea"}},
			Stages:              []stagesv1.Stage{{Name: "stage-a", SourceRef: stagesv1.SourceReference{Name: "stage-ea"}}},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	if err := reconcileWith(t, c, ss, nil); err != nil {
		t.Fatalf("baseline reconcile: %v", err)
	}
	base := getStageSet(t, c, ns, "broken")
	base.Spec.Version = &stagesv1.VersionSource{Value: "2.0.0"}
	if err := c.Update(context.Background(), base); err != nil {
		t.Fatalf("bump version: %v", err)
	}
	// The transition fetches the malformed ladder; the reconcile reports a
	// terminal failure (no requeue) rather than erroring out.
	_ = reconcileWith(t, c, base, nil)

	final := getStageSet(t, c, ns, "broken")
	if r := readyReason(final); r != ReasonMigrationArtifactInvalid {
		t.Fatalf("Ready reason = %q, want %q", r, ReasonMigrationArtifactInvalid)
	}
	if final.Status.Version != "1.0.0" {
		t.Fatalf("version must not advance on an invalid ladder, got %q", final.Status.Version)
	}
}
