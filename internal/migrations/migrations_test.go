// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package migrations

import (
	"testing"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

func TestParseLadder(t *testing.T) {
	t.Parallel()
	list := "- name: a\n  to: \"2.0.0\"\n  stage: deploy\n- name: b\n  to: \"3.0.0\"\n  stage: deploy\n"
	single := "name: solo\nto: \"2.0.0\"\nstage: deploy\n"

	t.Run("a list file yields every entry", func(t *testing.T) {
		got, err := ParseLadder(map[string]string{"ladder.yaml": list})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || got[0].Name != "a" || got[1].Name != "b" {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("a single-migration file yields one entry", func(t *testing.T) {
		got, err := ParseLadder(map[string]string{"m.yaml": single})
		if err != nil || len(got) != 1 || got[0].Name != "solo" {
			t.Fatalf("got %+v, err %v", got, err)
		}
	})

	t.Run("multiple files merge in sorted path order", func(t *testing.T) {
		got, err := ParseLadder(map[string]string{
			"02-b.yaml": "name: b\nto: \"3.0.0\"\nstage: deploy\n",
			"01-a.yaml": "name: a\nto: \"2.0.0\"\nstage: deploy\n",
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || got[0].Name != "a" || got[1].Name != "b" {
			t.Fatalf("not sorted by path: %+v", got)
		}
	})

	t.Run("non-migration extensions are ignored", func(t *testing.T) {
		got, err := ParseLadder(map[string]string{"README.md": "# hi", "m.json": `[{"name":"a","to":"2.0.0","stage":"deploy"}]`})
		if err != nil || len(got) != 1 || got[0].Name != "a" {
			t.Fatalf("got %+v, err %v", got, err)
		}
	})

	t.Run("no parseable files is an error", func(t *testing.T) {
		if _, err := ParseLadder(map[string]string{"README.md": "# hi"}); err == nil {
			t.Fatal("expected error for no migration files")
		}
	})

	t.Run("empty content defines no migrations", func(t *testing.T) {
		if _, err := ParseLadder(map[string]string{"empty.yaml": ""}); err == nil {
			t.Fatal("expected error for an empty ladder")
		}
	})

	t.Run("malformed yaml is an error", func(t *testing.T) {
		if _, err := ParseLadder(map[string]string{"bad.yaml": "name: [unterminated"}); err == nil {
			t.Fatal("expected parse error")
		}
	})

	t.Run("a misspelled field is rejected (strict)", func(t *testing.T) {
		if _, err := ParseLadder(map[string]string{"typo.yaml": "name: a\nto: \"2.0.0\"\nacions: []\n"}); err == nil {
			t.Fatal("expected strict-parse error for unknown field")
		}
	})
}

func TestValidateLadder(t *testing.T) {
	t.Parallel()
	ok := func(a stagesv1.Action) []stagesv1.Action { return []stagesv1.Action{a} }
	del := stagesv1.Action{Name: "x", Delete: &stagesv1.DeleteAction{}}

	tests := []struct {
		name    string
		ladder  []stagesv1.Migration
		wantErr bool
	}{
		{name: "valid", ladder: []stagesv1.Migration{{Name: "a", To: "2.0.0", From: ">=1.0.0, <2.0.0", Actions: ok(del)}}},
		{name: "empty migration name", ladder: []stagesv1.Migration{{Name: "", To: "2.0.0"}}, wantErr: true},
		{name: "empty to", ladder: []stagesv1.Migration{{Name: "a", To: ""}}, wantErr: true},
		{name: "non-semver to", ladder: []stagesv1.Migration{{Name: "a", To: "not-a-version"}}, wantErr: true},
		{name: "invalid from constraint", ladder: []stagesv1.Migration{{Name: "a", To: "2.0.0", From: ">>bad"}}, wantErr: true},
		{
			name:    "duplicate migration name",
			ladder:  []stagesv1.Migration{{Name: "a", To: "2.0.0"}, {Name: "a", To: "3.0.0"}},
			wantErr: true,
		},
		{
			name:    "action with no verb",
			ladder:  []stagesv1.Migration{{Name: "a", To: "2.0.0", Actions: ok(stagesv1.Action{Name: "x"})}},
			wantErr: true,
		},
		{
			name: "duplicate action name",
			ladder: []stagesv1.Migration{{Name: "a", To: "2.0.0", Actions: []stagesv1.Action{
				{Name: "dup", Delete: &stagesv1.DeleteAction{}},
				{Name: "dup", Wait: &stagesv1.WaitAction{}},
			}}},
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := ValidateLadder(tc.ladder); (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}
