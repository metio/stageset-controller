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

// ValidateLadder applies the per-migration content checks plus migration-name
// uniqueness across the whole ladder — the admission-time invariants that, for a
// sourced ladder, can only run at fetch time because the content isn't in the spec.
func ValidateLadder(ladder []stagesv1.Migration) error {
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
	}
	return nil
}

// semverIsBare reports whether s is a plain X.Y.Z version with no constraint
// operator (>=, =, ~, ^, a wildcard, or a range). As a `from` such a value
// matches only the exact version, which is rarely what an author intends.
func semverIsBare(s string) bool {
	_, err := semver.StrictNewVersion(s)
	return err == nil
}
