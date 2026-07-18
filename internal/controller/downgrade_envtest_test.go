// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

// reversibleLadder is a one-migration ladder (to 2.0.0) whose up action deletes
// upTarget and whose down action deletes downTarget — two observable side effects
// that prove which direction ran.
func reversibleLadder(ns, upTarget, downTarget string) string {
	return "" +
		"- name: schema-2\n" +
		"  to: \"2.0.0\"\n" +
		"  stage: stage-a\n" +
		"  actions:\n" +
		"    - name: apply-schema\n" +
		"      delete:\n" +
		"        target:\n" +
		"          apiVersion: v1\n" +
		"          kind: ConfigMap\n" +
		"          name: " + upTarget + "\n" +
		"          namespace: " + ns + "\n" +
		"  down:\n" +
		"    - name: revert-schema\n" +
		"      delete:\n" +
		"        target:\n" +
		"          apiVersion: v1\n" +
		"          kind: ConfigMap\n" +
		"          name: " + downTarget + "\n" +
		"          namespace: " + ns + "\n"
}

// irreversibleLadder is a one-migration ladder (to 2.0.0) with an up action but no
// down actions, so it is irreversible.
func irreversibleLadder(ns, upTarget string) string {
	return "" +
		"- name: schema-2\n" +
		"  to: \"2.0.0\"\n" +
		"  stage: stage-a\n" +
		"  actions:\n" +
		"    - name: apply-schema\n" +
		"      delete:\n" +
		"        target:\n" +
		"          apiVersion: v1\n" +
		"          kind: ConfigMap\n" +
		"          name: " + upTarget + "\n" +
		"          namespace: " + ns + "\n"
}

// upgradeTo2 drives a StageSet from a 1.0.0 baseline up to 2.0.0, crossing the
// schema-2 migration, and returns the refreshed object at 2.0.0. It asserts the
// up action ran.
func upgradeTo2(t *testing.T, c client.Client, ns, name string) *stagesv1.StageSet {
	t.Helper()
	ss := getStageSet(t, c, ns, name)
	if err := reconcileWith(t, c, ss, nil); err != nil {
		t.Fatalf("baseline reconcile: %v", err)
	}
	ss = getStageSet(t, c, ns, name)
	if ss.Status.Version != "1.0.0" {
		t.Fatalf("baseline version = %q, want 1.0.0", ss.Status.Version)
	}
	ss.Spec.Version = &stagesv1.VersionSource{Value: "2.0.0"}
	if err := c.Update(context.Background(), ss); err != nil {
		t.Fatalf("bump to 2.0.0: %v", err)
	}
	if err := reconcileWith(t, c, ss, nil); err != nil {
		t.Fatalf("upgrade reconcile: %v", err)
	}
	ss = getStageSet(t, c, ns, name)
	if ss.Status.Version != "2.0.0" {
		t.Fatalf("upgrade version = %q, want 2.0.0", ss.Status.Version)
	}
	return ss
}

// TestReconcile_Downgrade_RunsDownActionsAndLowersVersion proves the whole
// reversible-downgrade path: after an upgrade to 2.0.0 (the up action ran), an
// allowed downgrade to 1.0.0 runs the migration's down action and lowers
// status.version.
func TestReconcile_Downgrade_RunsDownActionsAndLowersVersion(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)

	// The up action deletes up-victim; the down action deletes down-victim.
	for _, n := range []string{"up-victim", "down-victim"} {
		if err := c.Create(context.Background(), &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: n}}); err != nil {
			t.Fatalf("create %s: %v", n, err)
		}
	}
	servedArtifact(t, c, ns, "stage-ea", "", map[string]string{"cm.yaml": configMapManifest(ns, "stage-obj")})
	servedArtifact(t, c, ns, "ladder-ea", "", map[string]string{"ladder.yaml": reversibleLadder(ns, "up-victim", "down-victim")})

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "app"},
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

	up := upgradeTo2(t, c, ns, "app")
	if cmExists(t, c, ns, "up-victim") {
		t.Fatal("the up migration did not run (up-victim still exists)")
	}
	if !cmExists(t, c, ns, "down-victim") {
		t.Fatal("the down action must not run on the way up (down-victim gone)")
	}

	// Enable downgrades and drop the target to 1.0.0.
	up.Spec.Version = &stagesv1.VersionSource{Value: "1.0.0", AllowDowngrade: true}
	if err := c.Update(context.Background(), up); err != nil {
		t.Fatalf("request downgrade: %v", err)
	}
	if err := reconcileWith(t, c, up, nil); err != nil {
		t.Fatalf("downgrade reconcile: %v", err)
	}

	final := getStageSet(t, c, ns, "app")
	if final.Status.Version != "1.0.0" {
		t.Fatalf("version did not lower on downgrade: %q", final.Status.Version)
	}
	if cmExists(t, c, ns, "down-victim") {
		t.Fatal("the down action did not run (down-victim still exists)")
	}
	if r := readyReason(final); r != ReasonReady {
		t.Fatalf("Ready reason after downgrade = %q, want %q", r, ReasonReady)
	}
	if len(final.Status.ExecutedMigrations) != 0 {
		t.Fatalf("ledger should clear after the downgrade, got %v", final.Status.ExecutedMigrations)
	}
}

// twoBoundaryLadder crosses 2.0.0 and 3.0.0, each with a down action that deletes
// its own marker, so a multi-version downgrade must reverse both.
func twoBoundaryLadder(ns string) string {
	mig := func(name, to, downTarget string) string {
		return "" +
			"- name: " + name + "\n" +
			"  to: \"" + to + "\"\n" +
			"  stage: stage-a\n" +
			"  actions:\n" +
			"    - name: up-" + name + "\n" +
			"      delete:\n" +
			"        target:\n" +
			"          apiVersion: v1\n" +
			"          kind: ConfigMap\n" +
			"          name: absent-" + name + "\n" +
			"          namespace: " + ns + "\n" +
			"  down:\n" +
			"    - name: down-" + name + "\n" +
			"      delete:\n" +
			"        target:\n" +
			"          apiVersion: v1\n" +
			"          kind: ConfigMap\n" +
			"          name: " + downTarget + "\n" +
			"          namespace: " + ns + "\n"
	}
	return mig("schema-2", "2.0.0", "marker-2") + mig("schema-3", "3.0.0", "marker-3")
}

// TestReconcile_Downgrade_UnwindsEveryCrossedBoundary proves a multi-version
// downgrade (3.0.0 → 1.0.0) reverses every boundary it crosses: both markers are
// deleted and the version lands at 1.0.0.
func TestReconcile_Downgrade_UnwindsEveryCrossedBoundary(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	for _, n := range []string{"marker-2", "marker-3"} {
		if err := c.Create(context.Background(), &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: n}}); err != nil {
			t.Fatalf("create %s: %v", n, err)
		}
	}
	servedArtifact(t, c, ns, "stage-ea", "", map[string]string{"cm.yaml": configMapManifest(ns, "stage-obj")})
	servedArtifact(t, c, ns, "ladder-ea", "", map[string]string{"ladder.yaml": twoBoundaryLadder(ns)})
	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "app"},
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
	// Baseline at 1.0.0, then climb to 3.0.0 (crosses both boundaries).
	if err := reconcileWith(t, c, ss, nil); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	ss = getStageSet(t, c, ns, "app")
	ss.Spec.Version = &stagesv1.VersionSource{Value: "3.0.0"}
	if err := c.Update(context.Background(), ss); err != nil {
		t.Fatalf("bump to 3.0.0: %v", err)
	}
	if err := reconcileWith(t, c, ss, nil); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	ss = getStageSet(t, c, ns, "app")
	if ss.Status.Version != "3.0.0" {
		t.Fatalf("upgrade version = %q, want 3.0.0", ss.Status.Version)
	}

	// Downgrade 3.0.0 → 1.0.0 crosses both 3.0.0 and 2.0.0.
	ss.Spec.Version = &stagesv1.VersionSource{Value: "1.0.0", AllowDowngrade: true}
	if err := c.Update(context.Background(), ss); err != nil {
		t.Fatalf("request downgrade: %v", err)
	}
	if err := reconcileWith(t, c, ss, nil); err != nil {
		t.Fatalf("downgrade: %v", err)
	}
	final := getStageSet(t, c, ns, "app")
	if final.Status.Version != "1.0.0" {
		t.Fatalf("version did not lower: %q", final.Status.Version)
	}
	if cmExists(t, c, ns, "marker-2") {
		t.Fatal("the 2.0.0 boundary's down action did not run (marker-2 remains)")
	}
	if cmExists(t, c, ns, "marker-3") {
		t.Fatal("the 3.0.0 boundary's down action did not run (marker-3 remains)")
	}
}

// TestReconcile_RollbackToAnnotation_TriggersDowngrade proves the rollback-to
// annotation overrides the source-resolved version with a lower one, driving a
// downgrade — the mechanism a FleetRollout uses to revert a regressed member.
func TestReconcile_RollbackToAnnotation_TriggersDowngrade(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	for _, n := range []string{"up-victim", "down-victim"} {
		if err := c.Create(context.Background(), &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: n}}); err != nil {
			t.Fatalf("create %s: %v", n, err)
		}
	}
	servedArtifact(t, c, ns, "stage-ea", "", map[string]string{"cm.yaml": configMapManifest(ns, "stage-obj")})
	servedArtifact(t, c, ns, "ladder-ea", "", map[string]string{"ladder.yaml": reversibleLadder(ns, "up-victim", "down-victim")})
	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "app"},
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
	// Baseline at 1.0.0, then upgrade to 2.0.0 with downgrades allowed.
	if err := reconcileWith(t, c, ss, nil); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	ss = getStageSet(t, c, ns, "app")
	ss.Spec.Version = &stagesv1.VersionSource{Value: "2.0.0", AllowDowngrade: true}
	if err := c.Update(context.Background(), ss); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	if err := reconcileWith(t, c, ss, nil); err != nil {
		t.Fatalf("upgrade reconcile: %v", err)
	}
	if v := getStageSet(t, c, ns, "app").Status.Version; v != "2.0.0" {
		t.Fatalf("upgrade version = %q, want 2.0.0", v)
	}

	// Stamp rollback-to below the deployed version. The source still says 2.0.0,
	// but the directive overrides it → downgrade to 1.0.0 runs the down action.
	ss = getStageSet(t, c, ns, "app")
	if ss.Annotations == nil {
		ss.Annotations = map[string]string{}
	}
	ss.Annotations[rollbackToAnnotation] = "1.0.0"
	if err := c.Update(context.Background(), ss); err != nil {
		t.Fatalf("stamp rollback-to: %v", err)
	}
	if err := reconcileWith(t, c, ss, nil); err != nil {
		t.Fatalf("rollback reconcile: %v", err)
	}
	final := getStageSet(t, c, ns, "app")
	if final.Status.Version != "1.0.0" {
		t.Fatalf("rollback-to did not downgrade: version = %q, want 1.0.0", final.Status.Version)
	}
	if cmExists(t, c, ns, "down-victim") {
		t.Fatal("the down action did not run under the rollback directive")
	}
}

// TestReconcile_Downgrade_RefusedWithoutAllowDowngrade proves a downgrade is
// refused with ReasonDowngradeNotAllowed when allowDowngrade is unset, and the
// version does not move.
func TestReconcile_Downgrade_RefusedWithoutAllowDowngrade(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	for _, n := range []string{"up-victim", "down-victim"} {
		if err := c.Create(context.Background(), &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: n}}); err != nil {
			t.Fatalf("create %s: %v", n, err)
		}
	}
	servedArtifact(t, c, ns, "stage-ea", "", map[string]string{"cm.yaml": configMapManifest(ns, "stage-obj")})
	servedArtifact(t, c, ns, "ladder-ea", "", map[string]string{"ladder.yaml": reversibleLadder(ns, "up-victim", "down-victim")})
	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "app"},
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
	up := upgradeTo2(t, c, ns, "app")

	// Downgrade with allowDowngrade unset.
	up.Spec.Version = &stagesv1.VersionSource{Value: "1.0.0"}
	if err := c.Update(context.Background(), up); err != nil {
		t.Fatalf("request downgrade: %v", err)
	}
	if err := reconcileWith(t, c, up, nil); err != nil {
		t.Fatalf("downgrade reconcile: %v", err)
	}
	final := getStageSet(t, c, ns, "app")
	if r := readyReason(final); r != ReasonDowngradeNotAllowed {
		t.Fatalf("Ready reason = %q, want %q", r, ReasonDowngradeNotAllowed)
	}
	if final.Status.Version != "2.0.0" {
		t.Fatalf("a refused downgrade must not move the version, got %q", final.Status.Version)
	}
	if !cmExists(t, c, ns, "down-victim") {
		t.Fatal("a refused downgrade must not run the down action (down-victim was deleted)")
	}
}

// TestReconcile_Downgrade_IrreversibleBoundaryRefused proves a downgrade across a
// boundary with no down actions is refused with ReasonDowngradeRequiresMigration,
// names the boundary, and does not move the version.
func TestReconcile_Downgrade_IrreversibleBoundaryRefused(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	if err := c.Create(context.Background(), &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "up-victim"}}); err != nil {
		t.Fatalf("create up-victim: %v", err)
	}
	servedArtifact(t, c, ns, "stage-ea", "", map[string]string{"cm.yaml": configMapManifest(ns, "stage-obj")})
	servedArtifact(t, c, ns, "ladder-ea", "", map[string]string{"ladder.yaml": irreversibleLadder(ns, "up-victim")})
	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "app"},
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
	up := upgradeTo2(t, c, ns, "app")

	up.Spec.Version = &stagesv1.VersionSource{Value: "1.0.0", AllowDowngrade: true}
	if err := c.Update(context.Background(), up); err != nil {
		t.Fatalf("request downgrade: %v", err)
	}
	if err := reconcileWith(t, c, up, nil); err != nil {
		t.Fatalf("downgrade reconcile: %v", err)
	}
	final := getStageSet(t, c, ns, "app")
	if r := readyReason(final); r != ReasonDowngradeRequiresMigration {
		t.Fatalf("Ready reason = %q, want %q", r, ReasonDowngradeRequiresMigration)
	}
	if msg := readyMessageOf(final); !strings.Contains(msg, "schema-2") || !strings.Contains(msg, "irreversible") {
		t.Fatalf("message should name the irreversible boundary, got %q", msg)
	}
	if final.Status.Version != "2.0.0" {
		t.Fatalf("a refused downgrade must not move the version, got %q", final.Status.Version)
	}
}
