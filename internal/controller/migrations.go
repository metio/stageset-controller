// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/Masterminds/semver/v3"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/util/jsonpath"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/actions"
	"github.com/metio/stageset-controller/internal/artifact"
	"github.com/metio/stageset-controller/internal/build"
)

// versionLabel is the Kubernetes-recommended label carrying an application's
// version. It is the default field spec.version.fromObject reads.
const versionLabel = "app.kubernetes.io/version"

// errInvalidVersion marks a terminal version-resolution failure (missing or
// unparseable version file / spec), as opposed to a transient fetch error.
var errInvalidVersion = errors.New("invalid version")

// migrationPlan is the version transition computed for one reconcile: the
// desired version, whether the controller is baselining (first adoption, no
// migrations), and the migrations to run, ordered by ascending target version.
type migrationPlan struct {
	versionSet bool
	desired    string
	baseline   bool
	pending    []*stagesv1.Migration
}

func (p *migrationPlan) pendingNames() []string {
	if len(p.pending) == 0 {
		return nil
	}
	out := make([]string, 0, len(p.pending))
	for _, m := range p.pending {
		out = append(out, m.Name)
	}
	return out
}

func (p *migrationPlan) forStage(stage string) []*stagesv1.Migration {
	var out []*stagesv1.Migration
	for _, m := range p.pending {
		if m.Stage == stage {
			out = append(out, m)
		}
	}
	return out
}

// planVersionMigrations resolves the desired version and the ordered migrations
// for the current transition. A nil reason means proceed; a non-empty reason is
// a terminal/transient Ready failure (InvalidVersion, DowngradeRequiresMigration)
// the caller surfaces. A non-nil error is a transient fetch failure (requeue).
func (r *StageSetReconciler) planVersionMigrations(ctx context.Context, ss *stagesv1.StageSet, resolved []artifact.ResolvedArtifact, fetcher *artifact.Fetcher) (*migrationPlan, string, string, error) {
	if ss.Spec.Version == nil {
		return &migrationPlan{versionSet: false}, "", "", nil
	}
	desired, err := r.resolveDesiredVersion(ctx, ss, resolved, fetcher)
	if err != nil {
		if errors.Is(err, errInvalidVersion) {
			return nil, ReasonInvalidVersion, err.Error(), nil
		}
		return nil, "", "", err // transient (fetch)
	}
	desiredV, err := semver.NewVersion(desired)
	if err != nil {
		return nil, ReasonInvalidVersion, fmt.Sprintf("desired version %q is not a semver: %v", desired, err), nil
	}
	plan := &migrationPlan{versionSet: true, desired: desired}

	current := ss.Status.Version
	if current == "" {
		// Baselining (Flyway-style): record the version, run no migrations.
		plan.baseline = true
		return plan, "", "", nil
	}
	currentV, err := semver.NewVersion(current)
	if err != nil {
		return nil, ReasonInvalidVersion, fmt.Sprintf("recorded status.version %q is not a semver: %v", current, err), nil
	}
	switch {
	case desiredV.LessThan(currentV):
		return nil, ReasonDowngradeRequiresMigration,
			fmt.Sprintf("desired version %s is below the deployed version %s; downgrades are refused", desired, current), nil
	case desiredV.Equal(currentV):
		return plan, "", "", nil // no transition
	}

	pending, perr := selectMigrations(ss.Spec.Migrations, currentV, desiredV)
	if perr != nil {
		return nil, ReasonInvalidVersion, perr.Error(), nil
	}
	plan.pending = pending
	return plan, "", "", nil
}

// selectMigrations returns the migrations whose target version is crossed by
// current -> desired (current < to <= desired) and whose optional `from`
// constraint the current version satisfies, ordered by ascending target.
func selectMigrations(migrations []stagesv1.Migration, currentV, desiredV *semver.Version) ([]*stagesv1.Migration, error) {
	type scored struct {
		m  *stagesv1.Migration
		to *semver.Version
	}
	var picked []scored
	for i := range migrations {
		m := &migrations[i]
		toV, err := semver.NewVersion(m.To)
		if err != nil {
			return nil, fmt.Errorf("%w: migration %q has invalid to %q", errInvalidVersion, m.Name, m.To)
		}
		if !toV.GreaterThan(currentV) || toV.GreaterThan(desiredV) {
			continue // boundary not crossed by this transition
		}
		if m.From != "" {
			constraint, err := semver.NewConstraint(m.From)
			if err != nil {
				return nil, fmt.Errorf("%w: migration %q has invalid from %q", errInvalidVersion, m.Name, m.From)
			}
			if !constraint.Check(currentV) {
				continue // current version does not satisfy the from constraint
			}
		}
		picked = append(picked, scored{m: m, to: toV})
	}
	// Ascending target version; equal targets keep spec order (stable sort).
	sort.SliceStable(picked, func(i, j int) bool { return picked[i].to.LessThan(picked[j].to) })
	out := make([]*stagesv1.Migration, 0, len(picked))
	for _, s := range picked {
		out = append(out, s.m)
	}
	return out, nil
}

// resolveDesiredVersion reads the desired version from spec.version: an inline
// value, a field of a rendered object (fromObject), or a file inside a named
// stage's artifact (fromArtifact). A missing stage/file/object or an empty value
// is a terminal errInvalidVersion; a fetch failure is transient.
func (r *StageSetReconciler) resolveDesiredVersion(ctx context.Context, ss *stagesv1.StageSet, resolved []artifact.ResolvedArtifact, fetcher *artifact.Fetcher) (string, error) {
	v := ss.Spec.Version
	switch {
	case v.Value != "":
		return strings.TrimSpace(v.Value), nil
	case v.FromObject != nil:
		return r.versionFromObject(ctx, ss, resolved, fetcher, v.FromObject)
	case v.FromArtifact != nil:
		return r.versionFromArtifact(ctx, ss, resolved, fetcher, v.FromArtifact)
	default:
		return "", fmt.Errorf("%w: spec.version sets none of value, fromObject, or fromArtifact", errInvalidVersion)
	}
}

// stageIndex returns the index of the named stage, or -1.
func stageIndex(ss *stagesv1.StageSet, name string) int {
	for i := range ss.Spec.Stages {
		if ss.Spec.Stages[i].Name == name {
			return i
		}
	}
	return -1
}

// versionFromArtifact reads a bare semver string from a file in a stage's
// fetched artifact.
func (r *StageSetReconciler) versionFromArtifact(ctx context.Context, ss *stagesv1.StageSet, resolved []artifact.ResolvedArtifact, fetcher *artifact.Fetcher, ref *stagesv1.ArtifactVersionRef) (string, error) {
	idx := stageIndex(ss, ref.Stage)
	if idx < 0 {
		return "", fmt.Errorf("%w: spec.version.fromArtifact.stage %q is not a stage", errInvalidVersion, ref.Stage)
	}
	ra := resolved[idx]
	files, err := fetcher.Fetch(ctx, ra.URL, ra.Digest, "")
	if err != nil {
		return "", fmt.Errorf("fetch version artifact for stage %q: %w", ref.Stage, err)
	}
	content, ok := files[ref.Path]
	if !ok {
		return "", fmt.Errorf("%w: version file %q not found in stage %q artifact", errInvalidVersion, ref.Path, ref.Stage)
	}
	ver := strings.TrimSpace(content)
	if ver == "" {
		return "", fmt.Errorf("%w: version file %q is empty", errInvalidVersion, ref.Path)
	}
	return ver, nil
}

// versionFromObject builds a stage's manifests and reads the version from a
// field of one rendered object — by default the app.kubernetes.io/version
// label, so the version travels inside the manifests regardless of source kind.
func (r *StageSetReconciler) versionFromObject(ctx context.Context, ss *stagesv1.StageSet, resolved []artifact.ResolvedArtifact, fetcher *artifact.Fetcher, ref *stagesv1.ObjectVersionRef) (string, error) {
	idx := stageIndex(ss, ref.Stage)
	if idx < 0 {
		return "", fmt.Errorf("%w: spec.version.fromObject.stage %q is not a stage", errInvalidVersion, ref.Stage)
	}
	ra := resolved[idx]
	files, err := fetcher.Fetch(ctx, ra.URL, ra.Digest, "")
	if err != nil {
		return "", fmt.Errorf("fetch version artifact for stage %q: %w", ref.Stage, err)
	}
	stage := &ss.Spec.Stages[idx]
	vars, err := r.resolvePostBuildVars(ctx, ss.Namespace, stage.PostBuild)
	if err != nil {
		return "", fmt.Errorf("resolve postBuild variables for version stage %q: %w", ref.Stage, err)
	}
	objects, err := build.Build(files, build.Options{Path: stage.Path, Patches: stage.Patches}, vars)
	if err != nil {
		return "", fmt.Errorf("%w: building stage %q to read its version failed: %v", errInvalidVersion, ref.Stage, err)
	}
	obj := findVersionObject(objects, ref)
	if obj == nil {
		return "", fmt.Errorf("%w: version object %s %q not found in stage %q manifests", errInvalidVersion, ref.Kind, ref.Name, ref.Stage)
	}
	ver, err := extractVersionField(obj, ref.FieldPath)
	if err != nil {
		return "", err
	}
	ver = strings.TrimSpace(ver)
	if ver == "" {
		return "", fmt.Errorf("%w: version field on %s %q in stage %q resolved to empty", errInvalidVersion, ref.Kind, ref.Name, ref.Stage)
	}
	return ver, nil
}

// findVersionObject returns the rendered object matching the ref's Kind and
// Name (and APIVersion when set), or nil.
func findVersionObject(objects []*unstructured.Unstructured, ref *stagesv1.ObjectVersionRef) *unstructured.Unstructured {
	for _, o := range objects {
		if o.GetKind() != ref.Kind || o.GetName() != ref.Name {
			continue
		}
		if ref.APIVersion != "" && o.GetAPIVersion() != ref.APIVersion {
			continue
		}
		return o
	}
	return nil
}

// extractVersionField reads the version string from an object. An empty
// fieldPath reads the app.kubernetes.io/version label; otherwise fieldPath is a
// kubectl-style JSONPath that must resolve to the bare version string.
func extractVersionField(obj *unstructured.Unstructured, fieldPath string) (string, error) {
	if fieldPath == "" {
		val, found, err := unstructured.NestedString(obj.Object, "metadata", "labels", versionLabel)
		if err != nil || !found {
			return "", fmt.Errorf("%w: %s %q has no %s label; set spec.version.fromObject.fieldPath to read the version from a different field",
				errInvalidVersion, obj.GetKind(), obj.GetName(), versionLabel)
		}
		return val, nil
	}
	jp := jsonpath.New("version").AllowMissingKeys(false)
	if err := jp.Parse(fieldPath); err != nil {
		return "", fmt.Errorf("%w: spec.version.fromObject.fieldPath %q is not valid JSONPath: %v", errInvalidVersion, fieldPath, err)
	}
	var buf strings.Builder
	if err := jp.Execute(&buf, obj.Object); err != nil {
		return "", fmt.Errorf("%w: evaluating fieldPath %q on %s %q: %v", errInvalidVersion, fieldPath, obj.GetKind(), obj.GetName(), err)
	}
	return buf.String(), nil
}

// runStageMigrations runs the pending migrations anchored to this stage, before
// the stage's own pre-actions, skipping any already in the in-flight ledger. It
// records each completed migration on the status and emits Events. A migration
// failure stops the run; the ledger persists so a retry skips finished ones.
func (r *StageSetReconciler) runStageMigrations(ctx context.Context, ss *stagesv1.StageSet, stage string, plan *migrationPlan, executor *actions.Executor) error {
	if plan == nil || plan.baseline {
		return nil
	}
	executed := toStringSet(ss.Status.ExecutedMigrations)
	for _, m := range plan.forStage(stage) {
		if executed[m.Name] {
			continue
		}
		r.event(ss, corev1.EventTypeNormal, eventReasonMigrationStarted,
			fmt.Sprintf("migration %q (to %s) starting", m.Name, m.To))
		if err := executor.Run(ctx, ss.Namespace, m.Actions, map[string]bool{}, func(string) error { return nil }); err != nil {
			r.event(ss, corev1.EventTypeWarning, eventReasonMigrationFailed,
				fmt.Sprintf("migration %q failed: %v", m.Name, err))
			return fmt.Errorf("migration %q: %w", m.Name, err)
		}
		ss.Status.ExecutedMigrations = append(ss.Status.ExecutedMigrations, m.Name)
		executed[m.Name] = true
		r.event(ss, corev1.EventTypeNormal, eventReasonMigrationCompleted,
			fmt.Sprintf("migration %q (to %s) completed", m.Name, m.To))
	}
	return nil
}

// Event reasons for migration progress (Event-only; the Ready condition uses
// ReasonStageFailed on a failed migration).
const (
	eventReasonMigrationStarted   = "MigrationStarted"
	eventReasonMigrationCompleted = "MigrationCompleted"
	eventReasonMigrationFailed    = "MigrationFailed"
)
