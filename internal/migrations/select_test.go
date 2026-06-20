// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package migrations

import (
	"fmt"
	"testing"

	"github.com/Masterminds/semver/v3"
	"pgregory.net/rapid"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

func TestSelect_OrdersByAscendingTargetAndHonorsBoundary(t *testing.T) {
	t.Parallel()
	migs := []stagesv1.Migration{
		{Name: "to-3", To: "3.0.0", Stage: "s"},
		{Name: "to-2b", To: "2.0.0", Stage: "s"},
		{Name: "to-2a", To: "2.0.0", Stage: "s"},
		{Name: "below", To: "1.0.0", Stage: "s"}, // not crossed by 1.0.0 -> 3.0.0
		{Name: "above", To: "4.0.0", Stage: "s"}, // beyond desired
	}
	got, err := Select(migs, semver.MustParse("1.0.0"), semver.MustParse("3.0.0"))
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	var names []string
	for _, m := range got {
		names = append(names, m.Name)
	}
	want := []string{"to-2b", "to-2a", "to-3"} // ascending target; equal targets keep ladder order
	if fmt.Sprint(names) != fmt.Sprint(want) {
		t.Fatalf("got %v, want %v", names, want)
	}
}

func TestSelect_FromConstraintFiltersByCurrent(t *testing.T) {
	t.Parallel()
	migs := []stagesv1.Migration{{Name: "gated", To: "2.0.0", From: ">=1.5.0", Stage: "s"}}
	got, err := Select(migs, semver.MustParse("1.2.0"), semver.MustParse("2.0.0"))
	if err != nil || len(got) != 0 {
		t.Fatalf("from-constraint should exclude: got %v err %v", got, err)
	}
	got, err = Select(migs, semver.MustParse("1.6.0"), semver.MustParse("2.0.0"))
	if err != nil || len(got) != 1 {
		t.Fatalf("from-constraint should include: got %v err %v", got, err)
	}
}

func TestExplain_ReportsExclusionReasons(t *testing.T) {
	t.Parallel()
	ladder := []stagesv1.Migration{
		{Name: "fires", To: "2.0.0", Stage: "s"},
		{Name: "out-of-range", To: "5.0.0", Stage: "s"},
		{Name: "from-excluded", To: "2.0.0", From: ">=1.5.0", Stage: "s"},
	}
	outcomes, err := Explain(ladder, semver.MustParse("1.2.0"), semver.MustParse("2.0.0"))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Outcome{}
	for _, o := range outcomes {
		got[o.Migration.Name] = o
	}
	if !got["fires"].Fires {
		t.Errorf("fires should fire: %+v", got["fires"])
	}
	if got["out-of-range"].Fires || got["out-of-range"].Reason == "" {
		t.Errorf("out-of-range should be excluded with a reason: %+v", got["out-of-range"])
	}
	if got["from-excluded"].Fires || got["from-excluded"].Reason == "" {
		t.Errorf("from-excluded should be excluded with a reason: %+v", got["from-excluded"])
	}
}

// TestSelect_Property generates ladders over random semver targets and a random
// current->desired transition and checks the selection invariants for arbitrary
// inputs: selected iff current < to <= desired AND the from-constraint admits
// current; ordered by ascending target; and every result is one of the inputs.
func TestSelect_Property(t *testing.T) {
	t.Parallel()
	ver := rapid.Custom(func(rt *rapid.T) string {
		return fmt.Sprintf("%d.%d.0",
			rapid.IntRange(0, 4).Draw(rt, "major"),
			rapid.IntRange(0, 4).Draw(rt, "minor"))
	})

	rapid.Check(t, func(rt *rapid.T) {
		currentV := semver.MustParse(ver.Draw(rt, "current"))
		desiredV := semver.MustParse(ver.Draw(rt, "desired"))

		n := rapid.IntRange(0, 6).Draw(rt, "count")
		migs := make([]stagesv1.Migration, 0, n)
		for i := 0; i < n; i++ {
			m := stagesv1.Migration{Name: fmt.Sprintf("m%d", i), To: ver.Draw(rt, fmt.Sprintf("to%d", i)), Stage: "s"}
			if rapid.Bool().Draw(rt, fmt.Sprintf("hasFrom%d", i)) {
				m.From = fmt.Sprintf(">=%d.0.0", rapid.IntRange(0, 4).Draw(rt, fmt.Sprintf("fromMajor%d", i)))
			}
			migs = append(migs, m)
		}

		got, err := Select(migs, currentV, desiredV)
		if err != nil {
			rt.Fatalf("Select on valid semver inputs returned error: %v", err)
		}

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

		gotSet := map[string]bool{}
		var prev *semver.Version
		for _, m := range got {
			if byName[m.Name] == nil {
				rt.Fatalf("Select returned a migration %q not in the input", m.Name)
			}
			if !wantSet[m.Name] {
				rt.Fatalf("Select included %q (to=%s) outside the crossed boundary or failing its from=%q", m.Name, m.To, m.From)
			}
			gotSet[m.Name] = true
			toV := semver.MustParse(m.To)
			if prev != nil && toV.LessThan(prev) {
				rt.Fatalf("Select result not ascending by target: %s after %s", m.To, prev)
			}
			prev = toV
		}
		for name := range wantSet {
			if !gotSet[name] {
				rt.Fatalf("Select dropped expected migration %q", name)
			}
		}
	})
}
