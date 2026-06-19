// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"fmt"
	"testing"

	"github.com/Masterminds/semver/v3"
	"pgregory.net/rapid"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

// TestSelectMigrations_Property generates migration ladders over random semver
// targets and a random current->desired transition, and checks the selection
// invariants hold for arbitrary inputs:
//
//   - selected iff current < to <= desired (the crossed boundary) AND the
//     optional from-constraint admits current;
//   - the result is ordered by ascending target version;
//   - every selected migration is one of the inputs (no fabrication).
func TestSelectMigrations_Property(t *testing.T) {
	t.Parallel()
	// A small version space keeps boundary crossings (and from-constraint hits)
	// frequent enough to exercise both inclusion and exclusion.
	ver := rapid.Custom(func(rt *rapid.T) string {
		return fmt.Sprintf("%d.%d.0",
			rapid.IntRange(0, 4).Draw(rt, "major"),
			rapid.IntRange(0, 4).Draw(rt, "minor"))
	})

	rapid.Check(t, func(rt *rapid.T) {
		current := ver.Draw(rt, "current")
		desired := ver.Draw(rt, "desired")
		currentV := semver.MustParse(current)
		desiredV := semver.MustParse(desired)

		n := rapid.IntRange(0, 6).Draw(rt, "count")
		migs := make([]stagesv1.Migration, 0, n)
		for i := 0; i < n; i++ {
			m := stagesv1.Migration{
				Name:  fmt.Sprintf("m%d", i),
				To:    ver.Draw(rt, fmt.Sprintf("to%d", i)),
				Stage: "s",
			}
			// Optionally attach a from-constraint pinning a minimum major.
			if rapid.Bool().Draw(rt, fmt.Sprintf("hasFrom%d", i)) {
				m.From = fmt.Sprintf(">=%d.0.0", rapid.IntRange(0, 4).Draw(rt, fmt.Sprintf("fromMajor%d", i)))
			}
			migs = append(migs, m)
		}

		got, err := selectMigrations(migs, currentV, desiredV)
		if err != nil {
			rt.Fatalf("selectMigrations on valid semver inputs returned error: %v", err)
		}

		// Compute the expected set independently from the same rule.
		byName := map[string]*stagesv1.Migration{}
		wantSet := map[string]bool{}
		for i := range migs {
			m := &migs[i]
			byName[m.Name] = m
			toV := semver.MustParse(m.To)
			if !toV.GreaterThan(currentV) || toV.GreaterThan(desiredV) {
				continue
			}
			if m.From != "" {
				c, cerr := semver.NewConstraint(m.From)
				if cerr != nil {
					rt.Fatalf("constructed from-constraint %q did not parse: %v", m.From, cerr)
				}
				if !c.Check(currentV) {
					continue
				}
			}
			wantSet[m.Name] = true
		}

		// Every selected migration is expected, originates from the input, and is
		// within the crossed boundary.
		gotSet := map[string]bool{}
		var prev *semver.Version
		for _, m := range got {
			if byName[m.Name] == nil {
				rt.Fatalf("selectMigrations returned a migration %q not in the input", m.Name)
			}
			if !wantSet[m.Name] {
				rt.Fatalf("selectMigrations included %q (to=%s) outside current=%s..desired=%s or failing its from=%q",
					m.Name, m.To, current, desired, m.From)
			}
			gotSet[m.Name] = true
			toV := semver.MustParse(m.To)
			if prev != nil && toV.LessThan(prev) {
				rt.Fatalf("selectMigrations result not ascending by target: %s after %s", m.To, prev)
			}
			prev = toV
		}
		// Nothing expected was dropped.
		for name := range wantSet {
			if !gotSet[name] {
				rt.Fatalf("selectMigrations dropped expected migration %q", name)
			}
		}
	})
}
