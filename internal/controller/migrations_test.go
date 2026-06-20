// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/fluxcd/pkg/apis/meta"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/artifact"
)

// --- selectMigrations (pure) ------------------------------------------------

func TestSelectMigrations_OrdersByAscendingTargetAndHonorsBoundary(t *testing.T) {
	t.Parallel()
	migs := []stagesv1.Migration{
		{Name: "to-3", To: "3.0.0", Stage: "s"},
		{Name: "to-2b", To: "2.0.0", Stage: "s"},
		{Name: "to-2a", To: "2.0.0", Stage: "s"},
		{Name: "below", To: "1.0.0", Stage: "s"}, // not crossed by 1.0.0 -> 3.0.0
		{Name: "above", To: "4.0.0", Stage: "s"}, // beyond desired
	}
	cur := semver.MustParse("1.0.0")
	des := semver.MustParse("3.0.0")
	got, err := selectMigrations(migs, cur, des)
	if err != nil {
		t.Fatalf("selectMigrations: %v", err)
	}
	var names []string
	for _, m := range got {
		names = append(names, m.Name)
	}
	want := []string{"to-2b", "to-2a", "to-3"} // ascending target; equal targets keep spec order
	if len(names) != len(want) {
		t.Fatalf("got %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("got %v, want %v", names, want)
		}
	}
}

func TestSelectMigrations_FromConstraintFiltersByCurrent(t *testing.T) {
	t.Parallel()
	migs := []stagesv1.Migration{
		{Name: "gated", To: "2.0.0", From: ">=1.5.0", Stage: "s"},
	}
	// current 1.2.0 does not satisfy >=1.5.0 → excluded
	got, err := selectMigrations(migs, semver.MustParse("1.2.0"), semver.MustParse("2.0.0"))
	if err != nil || len(got) != 0 {
		t.Fatalf("from-constraint should exclude: got %v err %v", got, err)
	}
	// current 1.6.0 satisfies → included
	got, err = selectMigrations(migs, semver.MustParse("1.6.0"), semver.MustParse("2.0.0"))
	if err != nil || len(got) != 1 {
		t.Fatalf("from-constraint should include: got %v err %v", got, err)
	}
}

func TestCoverageGap(t *testing.T) {
	t.Parallel()
	v := semver.MustParse
	cases := []struct {
		name    string
		require bool
		cur     string
		des     string
		pending int
		want    bool
	}{
		{"off → never gaps", false, "1.4.0", "2.0.0", 0, false},
		{"major + no migration → gap", true, "1.4.0", "2.0.0", 0, true},
		{"major + a migration → ok", true, "1.4.0", "2.0.0", 1, false},
		{"minor + no migration → ok", true, "1.4.0", "1.5.0", 0, false},
		{"patch + no migration → ok", true, "1.4.0", "1.4.1", 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := coverageGap(c.require, v(c.cur), v(c.des), c.pending); got != c.want {
				t.Fatalf("coverageGap = %v, want %v", got, c.want)
			}
		})
	}
}

// --- envtest ----------------------------------------------------------------

// requireMigrationCoverage holds a major-version bump that has no migration.
func TestReconcile_Migration_RequireCoverageBlocksMajor(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	servedArtifact(t, c, ns, "ea", "", map[string]string{"cm.yaml": configMapManifest(ns, "stage-obj")})
	mk := func(version string) *stagesv1.StageSet {
		return &stagesv1.StageSet{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "coverage"},
			Spec: stagesv1.StageSetSpec{
				Interval: metav1.Duration{Duration: time.Minute},
				Version:  &stagesv1.VersionSource{Value: version, RequireMigrationCoverage: true},
				Stages:   []stagesv1.Stage{{Name: "stage-a", SourceRef: stagesv1.SourceReference{Name: "ea"}}},
			},
		}
	}
	ss := mk("1.0.0")
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create: %v", err)
	}
	reconcileOnce(t, c, ss) // baseline 1.0.0

	cur := getStageSet(t, c, ns, "coverage")
	cur.Spec.Version = &stagesv1.VersionSource{Value: "2.0.0", RequireMigrationCoverage: true}
	if err := c.Update(context.Background(), cur); err != nil {
		t.Fatalf("bump: %v", err)
	}
	reconcileOnce(t, c, cur)

	got := getStageSet(t, c, ns, "coverage")
	if readyReason(got) != ReasonMigrationCoverageMissing {
		t.Fatalf("Ready reason = %q, want %q", readyReason(got), ReasonMigrationCoverageMissing)
	}
	if got.Status.Version != "1.0.0" {
		t.Fatalf("an uncovered major bump must not advance the version, got %q", got.Status.Version)
	}
}

func deleteMigration(name, to, stage, targetName, ns string) stagesv1.Migration {
	return stagesv1.Migration{
		Name: name, To: to, Stage: stage,
		Actions: []stagesv1.Action{{
			Name:   name + "-drop",
			Delete: &stagesv1.DeleteAction{Target: meta.NamespacedObjectKindReference{APIVersion: "v1", Kind: "ConfigMap", Name: targetName, Namespace: ns}},
		}},
	}
}

func versionedStageSet(ns, name, eaName, version string, migs []stagesv1.Migration) *stagesv1.StageSet {
	return &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: stagesv1.StageSetSpec{
			Interval:   metav1.Duration{Duration: time.Minute},
			Version:    &stagesv1.VersionSource{Value: version},
			Migrations: migs,
			Stages:     []stagesv1.Stage{{Name: "stage-a", SourceRef: stagesv1.SourceReference{Name: eaName}}},
		},
	}
}

func setVersion(t *testing.T, c client.Client, ns, name, version string) *stagesv1.StageSet {
	t.Helper()
	ss := getStageSet(t, c, ns, name)
	ss.Spec.Version = &stagesv1.VersionSource{Value: version}
	if err := c.Update(context.Background(), ss); err != nil {
		t.Fatalf("update version: %v", err)
	}
	return ss
}

// First adoption records the version and runs NO migrations (Flyway-style).
func TestReconcile_Migration_BaselinesWithoutRunning(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	servedArtifact(t, c, ns, "ea", "", map[string]string{"cm.yaml": configMapManifest(ns, "stage-obj")})
	legacy := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "legacy"}}
	if err := c.Create(context.Background(), legacy); err != nil {
		t.Fatalf("create legacy: %v", err)
	}

	ss := versionedStageSet(ns, "baselined", "ea", "2.0.0", []stagesv1.Migration{deleteMigration("drop-legacy", "2.0.0", "stage-a", "legacy", ns)})
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	reconcileOnce(t, c, ss)

	got := getStageSet(t, c, ns, "baselined")
	if got.Status.Version != "2.0.0" {
		t.Fatalf("baseline should record version, got %q", got.Status.Version)
	}
	if !cmExists(t, c, ns, "legacy") {
		t.Fatal("baselining must not run migrations")
	}
}

// Crossing a version boundary runs the migration anchored to its stage.
func TestReconcile_Migration_RunsOnUpgrade(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	servedArtifact(t, c, ns, "ea", "", map[string]string{"cm.yaml": configMapManifest(ns, "stage-obj")})
	legacy := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "legacy"}}
	if err := c.Create(context.Background(), legacy); err != nil {
		t.Fatalf("create legacy: %v", err)
	}

	ss := versionedStageSet(ns, "upgrader", "ea", "1.0.0", []stagesv1.Migration{deleteMigration("drop-legacy", "2.0.0", "stage-a", "legacy", ns)})
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	reconcileOnce(t, c, ss) // baseline to 1.0.0
	if !cmExists(t, c, ns, "legacy") {
		t.Fatal("baseline run must not delete legacy")
	}

	setVersion(t, c, ns, "upgrader", "2.0.0")
	ss = getStageSet(t, c, ns, "upgrader")
	reconcileOnce(t, c, ss) // transition 1.0.0 -> 2.0.0

	var gone corev1.ConfigMap
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "legacy"}, &gone); !apierrors.IsNotFound(err) {
		t.Fatalf("the migration should have deleted legacy, get err = %v", err)
	}
	if v := getStageSet(t, c, ns, "upgrader").Status.Version; v != "2.0.0" {
		t.Fatalf("version should advance to 2.0.0, got %q", v)
	}
}

// A downgrade is refused.
func TestReconcile_Migration_DowngradeRefused(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	servedArtifact(t, c, ns, "ea", "", map[string]string{"cm.yaml": configMapManifest(ns, "stage-obj")})

	ss := versionedStageSet(ns, "downgrader", "ea", "2.0.0", nil)
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	reconcileOnce(t, c, ss) // baseline to 2.0.0

	setVersion(t, c, ns, "downgrader", "1.0.0")
	ss = getStageSet(t, c, ns, "downgrader")
	reconcileOnce(t, c, ss)

	got := getStageSet(t, c, ns, "downgrader")
	if readyReason(got) != ReasonDowngradeRequiresMigration {
		t.Fatalf("Ready reason = %q, want %q", readyReason(got), ReasonDowngradeRequiresMigration)
	}
	if got.Status.Version != "2.0.0" {
		t.Fatalf("a refused downgrade must not change the recorded version, got %q", got.Status.Version)
	}
}

// The version can be read from a file inside a stage's artifact.
func TestReconcile_Migration_VersionFromArtifact(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	servedArtifact(t, c, ns, "ea", "", map[string]string{
		"cm.yaml": configMapManifest(ns, "stage-obj"),
		"VERSION": "1.5.0\n",
	})

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "from-artifact"},
		Spec: stagesv1.StageSetSpec{
			Interval: metav1.Duration{Duration: time.Minute},
			Version:  &stagesv1.VersionSource{FromArtifact: &stagesv1.ArtifactVersionRef{Stage: "stage-a", Path: "VERSION"}},
			Stages:   []stagesv1.Stage{{Name: "stage-a", SourceRef: stagesv1.SourceReference{Name: "ea"}}},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	reconcileOnce(t, c, ss)

	if v := getStageSet(t, c, ns, "from-artifact").Status.Version; v != "1.5.0" {
		t.Fatalf("version should be read from the artifact, got %q", v)
	}
}

// The version can be read from the app.kubernetes.io/version label of a rendered
// object, so it travels inside the manifests (the JaaS-rendered / any-source path).
func TestReconcile_Migration_VersionFromObject(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	labeled := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: stage-obj\n  namespace: " + ns +
		"\n  labels:\n    app.kubernetes.io/version: \"1.5.0\"\ndata:\n  key: value\n"
	servedArtifact(t, c, ns, "ea", "", map[string]string{"cm.yaml": labeled})

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "from-object"},
		Spec: stagesv1.StageSetSpec{
			Interval: metav1.Duration{Duration: time.Minute},
			Version: &stagesv1.VersionSource{FromObject: &stagesv1.ObjectVersionRef{
				Stage: "stage-a", Kind: "ConfigMap", Name: "stage-obj",
			}},
			Stages: []stagesv1.Stage{{Name: "stage-a", SourceRef: stagesv1.SourceReference{Name: "ea"}}},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	reconcileOnce(t, c, ss)

	if v := getStageSet(t, c, ns, "from-object").Status.Version; v != "1.5.0" {
		t.Fatalf("version should be read from the object's version label, got %q", v)
	}
}

// A missing version file is a terminal InvalidVersion.
func TestReconcile_Migration_MissingVersionFileIsInvalid(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	servedArtifact(t, c, ns, "ea", "", map[string]string{"cm.yaml": configMapManifest(ns, "stage-obj")})

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "no-version-file"},
		Spec: stagesv1.StageSetSpec{
			Interval: metav1.Duration{Duration: time.Minute},
			Version:  &stagesv1.VersionSource{FromArtifact: &stagesv1.ArtifactVersionRef{Stage: "stage-a", Path: "VERSION"}},
			Stages:   []stagesv1.Stage{{Name: "stage-a", SourceRef: stagesv1.SourceReference{Name: "ea"}}},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	reconcileOnce(t, c, ss)

	if r := readyReason(getStageSet(t, c, ns, "no-version-file")); r != ReasonInvalidVersion {
		t.Fatalf("Ready reason = %q, want %q", r, ReasonInvalidVersion)
	}
}

func TestValidateMigrations_RequiresVersionAndKnownStage(t *testing.T) {
	t.Parallel()
	base := func() *stagesv1.StageSet {
		return &stagesv1.StageSet{Spec: stagesv1.StageSetSpec{
			Stages: []stagesv1.Stage{{Name: "s", SourceRef: stagesv1.SourceReference{Name: "x"}}},
		}}
	}
	// migrations without version
	ss := base()
	ss.Spec.Migrations = []stagesv1.Migration{{Name: "m", To: "2.0.0", Stage: "s"}}
	if err := validateMigrations(ss); err == nil {
		t.Fatal("migrations without spec.version must be rejected")
	}
	// anchor to unknown stage
	ss = base()
	ss.Spec.Version = &stagesv1.VersionSource{Value: "1.0.0"}
	ss.Spec.Migrations = []stagesv1.Migration{{Name: "m", To: "2.0.0", Stage: "ghost"}}
	if err := validateMigrations(ss); err == nil {
		t.Fatal("migration anchored to an unknown stage must be rejected")
	}
	// valid
	ss = base()
	ss.Spec.Version = &stagesv1.VersionSource{Value: "1.0.0"}
	ss.Spec.Migrations = []stagesv1.Migration{{Name: "m", To: "2.0.0", Stage: "s"}}
	if err := validateMigrations(ss); err != nil {
		t.Fatalf("valid migrations rejected: %v", err)
	}
}

func TestAnchorStage(t *testing.T) {
	t.Parallel()
	ss := &stagesv1.StageSet{Spec: stagesv1.StageSetSpec{Stages: []stagesv1.Stage{
		{Name: "prepare", MigrationAnchor: "db-pre"},
		{Name: "rollout"},
	}}}
	cases := []struct{ anchor, want string }{
		{"", "prepare"},        // empty anchors before the first stage
		{"db-pre", "prepare"},  // by migrationAnchor alias
		{"rollout", "rollout"}, // by stage name
		{"ghost", ""},          // unresolved
	}
	for _, c := range cases {
		if got := anchorStage(ss, c.anchor); got != c.want {
			t.Errorf("anchorStage(%q) = %q, want %q", c.anchor, got, c.want)
		}
	}
}

func TestResolveAnchors(t *testing.T) {
	t.Parallel()
	ss := &stagesv1.StageSet{Spec: stagesv1.StageSetSpec{Stages: []stagesv1.Stage{
		{Name: "prepare", MigrationAnchor: "db-pre"},
		{Name: "rollout"},
	}}}

	t.Run("resolves alias, name, and empty (empty anchors to first stage)", func(t *testing.T) {
		plan := &migrationPlan{pending: []*stagesv1.Migration{
			{Name: "a", Stage: "db-pre"},
			{Name: "b", Stage: "rollout"},
			{Name: "c", Stage: ""},
		}}
		if reason, _ := resolveAnchors(ss, plan); reason != "" {
			t.Fatalf("unexpected reason %q", reason)
		}
		if got := len(plan.forStage("prepare")); got != 2 { // a (db-pre) + c (empty → first)
			t.Fatalf("prepare migrations = %d, want 2", got)
		}
		if got := len(plan.forStage("rollout")); got != 1 {
			t.Fatalf("rollout migrations = %d, want 1", got)
		}
	})

	t.Run("unresolved anchor fails closed", func(t *testing.T) {
		plan := &migrationPlan{pending: []*stagesv1.Migration{{Name: "x", Stage: "ghost"}}}
		if reason, _ := resolveAnchors(ss, plan); reason != ReasonMigrationStageNotFound {
			t.Fatalf("reason = %q, want %q", reason, ReasonMigrationStageNotFound)
		}
	})
}

func TestPendingDetails(t *testing.T) {
	t.Parallel()
	ss := &stagesv1.StageSet{Spec: stagesv1.StageSetSpec{Stages: []stagesv1.Stage{
		{Name: "prepare", MigrationAnchor: "db-pre"},
		{Name: "rollout"},
	}}}
	m := &stagesv1.Migration{Name: "drop", To: "2.0.0", From: "1.x", Stage: "db-pre", Actions: []stagesv1.Action{
		{Name: "del", Delete: &stagesv1.DeleteAction{}},
		{Name: "hold", Wait: &stagesv1.WaitAction{}},
	}}
	plan := &migrationPlan{pending: []*stagesv1.Migration{m}}

	got := plan.pendingDetails(ss)
	if len(got) != 1 {
		t.Fatalf("want 1 detail, got %d", len(got))
	}
	d := got[0]
	if d.Name != "drop" || d.To != "2.0.0" || d.From != "1.x" {
		t.Fatalf("detail = %+v", d)
	}
	if d.Stage != "prepare" { // the db-pre anchor resolves to the concrete stage
		t.Fatalf("resolved stage = %q, want prepare", d.Stage)
	}
	if !reflect.DeepEqual(d.Actions, []string{"delete", "wait"}) {
		t.Fatalf("action verbs = %v, want [delete wait]", d.Actions)
	}
	if d.Digest != migrationDigest(m) {
		t.Fatalf("digest = %q, want %q", d.Digest, migrationDigest(m))
	}
}

func TestResolveMigrationLadder_Inline(t *testing.T) {
	t.Parallel()
	r := &StageSetReconciler{}
	ss := &stagesv1.StageSet{Spec: stagesv1.StageSetSpec{
		Migrations: []stagesv1.Migration{{Name: "a", To: "2.0.0", Stage: "deploy"}},
	}}
	got, reason, _, err := r.resolveMigrationLadder(context.Background(), ss, nil)
	if err != nil || reason != "" {
		t.Fatalf("inline ladder: reason=%q err=%v", reason, err)
	}
	if len(got) != 1 || got[0].Name != "a" {
		t.Fatalf("inline ladder not returned verbatim: %+v", got)
	}
}

func TestMigrationDigest(t *testing.T) {
	t.Parallel()
	m := &stagesv1.Migration{Name: "m", To: "2.0.0", Actions: []stagesv1.Action{{Name: "a", Delete: &stagesv1.DeleteAction{}}}}
	again := *m
	if migrationDigest(m) != migrationDigest(&again) {
		t.Fatal("digest is not stable for identical content")
	}
	changed := *m
	changed.Actions = []stagesv1.Action{{Name: "a", Wait: &stagesv1.WaitAction{}}}
	if migrationDigest(m) == migrationDigest(&changed) {
		t.Fatal("digest did not change when the action content changed")
	}
}

func TestCheckMigrationSourceVerified(t *testing.T) {
	t.Parallel()
	yes, no := true, false
	cases := []struct {
		name     string
		verified *bool
		require  bool
		wantFail bool
	}{
		{"unset, not required → ok", nil, false, false},
		{"unset, required → fail", nil, true, true},
		{"verified true → ok", &yes, false, false},
		{"verified true, required → ok", &yes, true, false},
		{"verified false → always fail", &no, false, true},
		{"verified false, required → fail", &no, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := &StageSetReconciler{RequireVerifiedMigrationSources: c.require}
			reason, _ := r.checkMigrationSourceVerified(artifact.ResolvedArtifact{Verified: c.verified})
			if (reason != "") != c.wantFail {
				t.Fatalf("reason=%q wantFail=%v", reason, c.wantFail)
			}
		})
	}
}

func TestFailReason(t *testing.T) {
	t.Parallel()
	plain := errors.New("boom")
	forbidden := apierrors.NewForbidden(schema.GroupResource{Resource: "stagesets"}, "n", errors.New("no"))
	cases := []struct {
		name     string
		op       string
		cause    error
		reason   string
		terminal bool
	}{
		{"ordinary stage op", "apply", plain, ReasonStageFailed, false},
		{"migration retries", opMigration, plain, ReasonMigrationFailed, false},
		{"dirty halts", opMigrationDirty, plain, ReasonMigrationDirty, true},
		{"rbac beats migration op", opMigration, forbidden, ReasonRBACDenied, true},
		{"rbac beats dirty op", opMigrationDirty, forbidden, ReasonRBACDenied, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			reason, terminal := failReason(c.op, c.cause)
			if reason != c.reason || terminal != c.terminal {
				t.Fatalf("failReason(%q) = (%q,%v), want (%q,%v)", c.op, reason, terminal, c.reason, c.terminal)
			}
		})
	}
}

func TestCheckMigrationSourcePinned(t *testing.T) {
	t.Parallel()
	yes, no := true, false
	cases := []struct {
		name     string
		pinned   *bool
		require  bool
		wantFail bool
	}{
		{"exempt (nil), not required → ok", nil, false, false},
		{"exempt (nil), required → ok", nil, true, false},
		{"pinned, required → ok", &yes, true, false},
		{"mutable, not required → ok (warns)", &no, false, false},
		{"mutable, required → fail", &no, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := &StageSetReconciler{RequirePinnedMigrationSources: c.require}
			reason, _ := r.checkMigrationSourcePinned(&stagesv1.StageSet{}, artifact.ResolvedArtifact{Pinned: c.pinned})
			if (reason != "") != c.wantFail {
				t.Fatalf("reason=%q wantFail=%v", reason, c.wantFail)
			}
		})
	}
}

func TestLadderHasHTTP(t *testing.T) {
	t.Parallel()
	no := []stagesv1.Migration{{Name: "a", Actions: []stagesv1.Action{{Name: "x", Delete: &stagesv1.DeleteAction{}}}}}
	yes := []stagesv1.Migration{{Name: "a", Actions: []stagesv1.Action{{Name: "x", HTTP: &stagesv1.HTTPAction{}}}}}
	if ladderHasHTTP(no) {
		t.Fatal("no http action but reported true")
	}
	if !ladderHasHTTP(yes) {
		t.Fatal("http action present but reported false")
	}
}

func TestActionsDoneFor(t *testing.T) {
	t.Parallel()
	ledger := []string{"m@abc/a", "m@abc/b", "other@xyz/a", "m@def/a"}
	done := actionsDoneFor(ledger, "m@abc")
	if !done["a"] || !done["b"] || done["other"] || len(done) != 2 {
		t.Fatalf("actionsDoneFor = %v, want {a,b}", done)
	}
}

// fakeExecutor implements actionExecutor: it skips actions already in done,
// records each action it runs, and fails on any action name in failOn.
type fakeExecutor struct {
	failOn   map[string]bool
	doneSeen []map[string]bool // the done-set passed to each Run call
	ran      []string          // action names actually executed (not skipped)
}

func (f *fakeExecutor) Run(_ context.Context, _ string, acts []stagesv1.Action, done map[string]bool, record func(string) error) error {
	f.doneSeen = append(f.doneSeen, done)
	for i := range acts {
		a := &acts[i]
		if done[a.Name] {
			continue
		}
		if f.failOn[a.Name] {
			return errors.New("action " + a.Name + " failed")
		}
		f.ran = append(f.ran, a.Name)
		if err := record(a.Name); err != nil {
			return err
		}
	}
	return nil
}

func threeActionPlan() (*stagesv1.Migration, *migrationPlan) {
	m := &stagesv1.Migration{Name: "m", To: "2.0.0", Stage: "deploy", Actions: []stagesv1.Action{
		{Name: "a", Delete: &stagesv1.DeleteAction{}},
		{Name: "b", Delete: &stagesv1.DeleteAction{}},
		{Name: "c", Delete: &stagesv1.DeleteAction{}},
	}}
	plan := &migrationPlan{pending: []*stagesv1.Migration{m}, byStage: map[string][]*stagesv1.Migration{"deploy": {m}}}
	return m, plan
}

func TestRunStageMigrations_PerActionIdempotency(t *testing.T) {
	t.Parallel()
	r := &StageSetReconciler{}
	m, plan := threeActionPlan()
	migKey := migrationKey(m)
	ss := &stagesv1.StageSet{}

	// Run 1: action "b" fails. "a" completed and is recorded; "b"/"c" are not.
	fe1 := &fakeExecutor{failOn: map[string]bool{"b": true}}
	if err := r.runStageMigrations(context.Background(), ss, "deploy", plan, fe1); err == nil {
		t.Fatal("expected the migration to fail at action b")
	}
	if got := actionsDoneFor(ss.Status.ExecutedMigrationActions, migKey); !got["a"] || got["b"] || got["c"] {
		t.Fatalf("after failure, action ledger = %v, want only {a}", got)
	}
	if len(ss.Status.ExecutedMigrations) != 0 {
		t.Fatalf("a failed migration must not be marked done: %v", ss.Status.ExecutedMigrations)
	}

	// Run 2 (retry): nothing fails. "a" must be skipped (already done), only
	// "b" and "c" run, and the migration is marked fully done.
	fe2 := &fakeExecutor{}
	if err := r.runStageMigrations(context.Background(), ss, "deploy", plan, fe2); err != nil {
		t.Fatalf("retry failed: %v", err)
	}
	if !fe2.doneSeen[0]["a"] {
		t.Fatal("retry did not pass action a as already-done")
	}
	for _, name := range fe2.ran {
		if name == "a" {
			t.Fatal("retry re-ran the already-completed destructive action a")
		}
	}
	if !toStringSet(ss.Status.ExecutedMigrations)[migKey] {
		t.Fatalf("migration not marked done after retry: %v", ss.Status.ExecutedMigrations)
	}
}

func TestRunStageMigrations_FullyDoneSkipped(t *testing.T) {
	t.Parallel()
	r := &StageSetReconciler{}
	m, plan := threeActionPlan()
	ss := &stagesv1.StageSet{}
	ss.Status.ExecutedMigrations = []string{migrationKey(m)}
	fe := &fakeExecutor{}
	if err := r.runStageMigrations(context.Background(), ss, "deploy", plan, fe); err != nil {
		t.Fatal(err)
	}
	if len(fe.ran) != 0 {
		t.Fatalf("a fully-done migration must not run any actions: %v", fe.ran)
	}
}

func TestRunStageMigrations_ContentChangeReRuns(t *testing.T) {
	t.Parallel()
	r := &StageSetReconciler{}
	m, plan := threeActionPlan()
	ss := &stagesv1.StageSet{}
	// Ledger holds a DIFFERENT content digest for the same name → not a match.
	ss.Status.ExecutedMigrations = []string{m.Name + "@stale0000000"}
	fe := &fakeExecutor{}
	if err := r.runStageMigrations(context.Background(), ss, "deploy", plan, fe); err != nil {
		t.Fatal(err)
	}
	if len(fe.ran) != 3 {
		t.Fatalf("a changed migration must re-run all actions, ran %v", fe.ran)
	}
}
