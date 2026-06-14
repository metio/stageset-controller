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

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/actions"
	"github.com/metio/stageset-controller/internal/artifact"
)

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
// value, or a file inside a named stage's artifact. A missing stage/file or an
// empty value is a terminal errInvalidVersion; a fetch failure is transient.
func (r *StageSetReconciler) resolveDesiredVersion(ctx context.Context, ss *stagesv1.StageSet, resolved []artifact.ResolvedArtifact, fetcher *artifact.Fetcher) (string, error) {
	v := ss.Spec.Version
	if v.Value != "" {
		return strings.TrimSpace(v.Value), nil
	}
	if v.FromArtifact == nil {
		return "", fmt.Errorf("%w: spec.version sets neither value nor fromArtifact", errInvalidVersion)
	}
	idx := -1
	for i := range ss.Spec.Stages {
		if ss.Spec.Stages[i].Name == v.FromArtifact.Stage {
			idx = i
			break
		}
	}
	if idx < 0 {
		return "", fmt.Errorf("%w: spec.version.fromArtifact.stage %q is not a stage", errInvalidVersion, v.FromArtifact.Stage)
	}
	ra := resolved[idx]
	files, err := fetcher.Fetch(ctx, ra.URL, ra.Digest, "")
	if err != nil {
		return "", fmt.Errorf("fetch version artifact for stage %q: %w", v.FromArtifact.Stage, err)
	}
	content, ok := files[v.FromArtifact.Path]
	if !ok {
		return "", fmt.Errorf("%w: version file %q not found in stage %q artifact", errInvalidVersion, v.FromArtifact.Path, v.FromArtifact.Stage)
	}
	ver := strings.TrimSpace(content)
	if ver == "" {
		return "", fmt.Errorf("%w: version file %q is empty", errInvalidVersion, v.FromArtifact.Path)
	}
	return ver, nil
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
