// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

// Package migrations holds the migration-ladder parsing and validation shared by
// the reconciler (admission + sourced-artifact fetch) and the CLI (stageset
// lint-migrations), so author-time and run-time validation cannot drift.
package migrations

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Masterminds/semver/v3"
	"sigs.k8s.io/yaml"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

// fileExts are the artifact file extensions parsed as migration definitions;
// other files (READMEs, integrity manifests) are ignored.
var fileExts = map[string]bool{".yaml": true, ".yml": true, ".json": true}

// ParseLadder parses the fetched artifact files into one ladder. Each
// .yaml/.yml/.json file is a YAML/JSON document that is either a list of
// Migration or a single Migration; files are processed in sorted path order so
// the ladder is deterministic regardless of map iteration. Unknown extensions
// are ignored. Parsing is strict, so a misspelled field is rejected rather than
// silently dropped — important for a destructive ladder.
func ParseLadder(files map[string]string) ([]stagesv1.Migration, error) {
	paths := make([]string, 0, len(files))
	for p := range files {
		if fileExts[strings.ToLower(filepath.Ext(p))] {
			paths = append(paths, p)
		}
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("migrations artifact contains no .yaml, .yml, or .json files")
	}
	sort.Strings(paths)
	var ladder []stagesv1.Migration
	for _, p := range paths {
		var list []stagesv1.Migration
		if err := yaml.UnmarshalStrict([]byte(files[p]), &list); err == nil {
			ladder = append(ladder, list...)
			continue
		}
		var single stagesv1.Migration
		if err := yaml.UnmarshalStrict([]byte(files[p]), &single); err != nil {
			return nil, fmt.Errorf("parsing migrations file %q: %w", p, err)
		}
		ladder = append(ladder, single)
	}
	if len(ladder) == 0 {
		return nil, fmt.Errorf("migrations artifact defined no migrations")
	}
	return ladder, nil
}

// DoS caps on a migration ladder. A sourced ladder is remote-authored content
// the controller executes, so an unbounded ladder (or an action with unbounded
// retries) is a denial-of-service vector: these bound the work a single artifact
// can enqueue. The limits are generous — real ladders are far smaller — so they
// only ever trip on a runaway or hostile artifact.
const (
	MaxMigrationsPerLadder = 200
	MaxActionsPerMigration = 100
	MaxActionRetries       = 10
)

// ValidateLadder applies the per-migration content checks plus migration-name
// uniqueness across the whole ladder — the admission-time invariants that, for a
// sourced ladder, can only run at fetch time because the content isn't in the spec.
func ValidateLadder(ladder []stagesv1.Migration) error {
	if len(ladder) > MaxMigrationsPerLadder {
		return fmt.Errorf("migration ladder has %d migrations, exceeding the limit of %d", len(ladder), MaxMigrationsPerLadder)
	}
	names := make(map[string]bool, len(ladder))
	for i := range ladder {
		m := &ladder[i]
		if err := ValidateMigration(m); err != nil {
			return err
		}
		if names[m.Name] {
			return fmt.Errorf("duplicate migration name %q; names are the idempotency-ledger key and must be unique", m.Name)
		}
		names[m.Name] = true
	}
	return nil
}

// ValidateMigration checks a single migration is well-formed: a non-empty name,
// a `to` that parses as a concrete semver, an optional `from` that parses as a
// semver constraint, and each action setting exactly one verb with a unique
// non-empty name (action names are the idempotency-ledger key). Shared by the
// admission webhook (inline migrations) and the sourced-ladder content check.
func ValidateMigration(m *stagesv1.Migration) error {
	if m.Name == "" {
		return fmt.Errorf("a migration has an empty name")
	}
	if m.To == "" {
		return fmt.Errorf("migration %q has an empty to", m.Name)
	}
	if _, err := semver.NewVersion(m.To); err != nil {
		return fmt.Errorf("migration %q has an invalid to %q: %w", m.Name, m.To, err)
	}
	if m.From != "" {
		if _, err := semver.NewConstraint(m.From); err != nil {
			return fmt.Errorf("migration %q has an invalid from constraint %q: %w", m.Name, m.From, err)
		}
		// A bare version like "1.0.0" is a valid constraint, but it matches ONLY
		// that exact version — a common foot-gun for authors who mean "from 1.0.0
		// onward". Reject it so the intent must be explicit.
		if from := strings.TrimSpace(m.From); semverIsBare(from) {
			return fmt.Errorf("migration %q from %q is a bare version, which matches only that exact version; write %q (from this version onward) or %q (exactly that version) to make the intent explicit",
				m.Name, m.From, ">="+from, "="+from)
		}
	}
	if len(m.Actions) > MaxActionsPerMigration {
		return fmt.Errorf("migration %q has %d actions, exceeding the limit of %d", m.Name, len(m.Actions), MaxActionsPerMigration)
	}
	seen := make(map[string]bool, len(m.Actions))
	for j := range m.Actions {
		a := &m.Actions[j]
		if n := a.VerbCount(); n != 1 {
			return fmt.Errorf("migration %q action %q: exactly one verb must be set, found %d", m.Name, a.Name, n)
		}
		if a.Name == "" {
			return fmt.Errorf("migration %q has an action with an empty name; action names are the idempotency-ledger key and must be set", m.Name)
		}
		if seen[a.Name] {
			return fmt.Errorf("migration %q has duplicate action name %q; action names are the idempotency-ledger key and must be unique", m.Name, a.Name)
		}
		seen[a.Name] = true
		if a.Scope != "" {
			return fmt.Errorf("migration %q action %q sets scope; scope is valid only on a stage's pre/post actions — a migration action is already keyed to its version transition by the migration ledger", m.Name, a.Name)
		}
		if a.Retries != nil && (*a.Retries < 0 || *a.Retries > MaxActionRetries) {
			return fmt.Errorf("migration %q action %q has retries %d; it must be between 0 and %d", m.Name, a.Name, *a.Retries, MaxActionRetries)
		}
	}
	return nil
}

// semverIsBare reports whether s is a plain X.Y.Z version with no constraint
// operator (>=, =, ~, ^, a wildcard, or a range). As a `from` such a value
// matches only the exact version, which is rarely what an author intends.
//
// A leading "v" is stripped first: Masterminds/semver accepts "v1.0.0" as a
// constraint (an exact match, the same foot-gun as the bare form), but
// StrictNewVersion rejects the "v" — so without this the v-prefixed spelling
// would slip past the guard.
func semverIsBare(s string) bool {
	s = strings.TrimPrefix(s, "v")
	s = strings.TrimPrefix(s, "V")
	_, err := semver.StrictNewVersion(s)
	return err == nil
}

// Outcome reports whether a current→desired transition selects a migration, and
// why not when it doesn't.
type Outcome struct {
	Migration *stagesv1.Migration
	Fires     bool
	Reason    string // why excluded; empty when Fires
}

// Explain reports, for every migration in the ladder, whether the transition
// current→desired selects it — the boundary is crossed (current < to <= desired)
// and the optional from-constraint admits current — ordered by ascending target
// version, the order they would run. It errors only on an unparseable to/from,
// which a ladder that passed ValidateLadder has none of. It is the shared
// selection logic behind both the reconciler's Select and the CLI's transition
// report, so the two never diverge.
func Explain(ladder []stagesv1.Migration, currentV, desiredV *semver.Version) ([]Outcome, error) {
	type scored struct {
		o  Outcome
		to *semver.Version
	}
	list := make([]scored, 0, len(ladder))
	for i := range ladder {
		m := &ladder[i]
		toV, err := semver.NewVersion(m.To)
		if err != nil {
			return nil, fmt.Errorf("migration %q has invalid to %q: %w", m.Name, m.To, err)
		}
		o := Outcome{Migration: m}
		switch {
		case !toV.GreaterThan(currentV) || toV.GreaterThan(desiredV):
			o.Reason = fmt.Sprintf("to %s is not in the crossed range (%s, %s]", m.To, currentV, desiredV)
		case m.From != "":
			constraint, err := semver.NewConstraint(m.From)
			if err != nil {
				return nil, fmt.Errorf("migration %q has invalid from %q: %w", m.Name, m.From, err)
			}
			if !constraint.Check(currentV) {
				o.Reason = fmt.Sprintf("from %q does not match current %s", m.From, currentV)
			}
		}
		o.Fires = o.Reason == ""
		list = append(list, scored{o: o, to: toV})
	}
	// Ascending target version; equal targets keep ladder order (stable sort).
	sort.SliceStable(list, func(i, j int) bool { return list[i].to.LessThan(list[j].to) })
	out := make([]Outcome, len(list))
	for i := range list {
		out[i] = list[i].o
	}
	return out, nil
}

// Select returns the migrations the transition current→desired fires, in run
// order (ascending target version). A thin filter over Explain.
func Select(ladder []stagesv1.Migration, currentV, desiredV *semver.Version) ([]*stagesv1.Migration, error) {
	outcomes, err := Explain(ladder, currentV, desiredV)
	if err != nil {
		return nil, err
	}
	var out []*stagesv1.Migration
	for i := range outcomes {
		if outcomes[i].Fires {
			out = append(out, outcomes[i].Migration)
		}
	}
	return out, nil
}
