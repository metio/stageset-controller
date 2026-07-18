// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package cli

import (
	"bytes"
	"context"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	fluxmeta "github.com/fluxcd/pkg/apis/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/actionplan"
	"github.com/metio/stageset-controller/internal/apply"
	"github.com/metio/stageset-controller/internal/artifact"
	"github.com/metio/stageset-controller/internal/diffrender"
)

// --- colorEnabled ---

func TestColorEnabled(t *testing.T) {
	t.Setenv("NO_COLOR", "")

	t.Run("always", func(t *testing.T) {
		got, err := colorEnabled("always", &bytes.Buffer{})
		if err != nil || !got {
			t.Fatalf("always = %v,%v want true,nil", got, err)
		}
	})

	t.Run("never", func(t *testing.T) {
		got, err := colorEnabled("never", &bytes.Buffer{})
		if err != nil || got {
			t.Fatalf("never = %v,%v want false,nil", got, err)
		}
	})

	t.Run("invalid", func(t *testing.T) {
		got, err := colorEnabled("rainbow", &bytes.Buffer{})
		if err == nil {
			t.Fatalf("invalid mode should error, got %v", got)
		}
		if !strings.Contains(err.Error(), "invalid --color") {
			t.Errorf("error text = %q", err.Error())
		}
	})

	t.Run("auto non-terminal writer is off", func(t *testing.T) {
		// A bytes.Buffer is not an *os.File, so auto can never resolve to true.
		got, err := colorEnabled("auto", &bytes.Buffer{})
		if err != nil || got {
			t.Fatalf("auto/non-terminal = %v,%v want false,nil", got, err)
		}
	})

	t.Run("empty mode behaves like auto", func(t *testing.T) {
		got, err := colorEnabled("", &bytes.Buffer{})
		if err != nil || got {
			t.Fatalf("empty mode = %v,%v want false,nil", got, err)
		}
	})
}

func TestColorEnabled_NoColorHonored(t *testing.T) {
	// NO_COLOR set forces auto off even against a real terminal file. Using
	// os.Stdout exercises the *os.File branch without depending on it actually
	// being a tty in CI (NO_COLOR short-circuits before the term check).
	t.Setenv("NO_COLOR", "1")
	got, err := colorEnabled("auto", os.Stdout)
	if err != nil || got {
		t.Fatalf("auto with NO_COLOR = %v,%v want false,nil", got, err)
	}

	// NO_COLOR does not override an explicit --color=always.
	got, err = colorEnabled("always", os.Stdout)
	if err != nil || !got {
		t.Fatalf("always ignores NO_COLOR = %v,%v want true,nil", got, err)
	}
}

// --- describeAction ---

func dur(d time.Duration) *metav1.Duration { return &metav1.Duration{Duration: d} }

func TestDescribeAction_Table(t *testing.T) {
	ref := func(kind, name string) fluxmeta.NamespacedObjectKindReference {
		return fluxmeta.NamespacedObjectKindReference{Kind: kind, Name: name}
	}
	tests := []struct {
		name       string
		action     stagesv1.Action
		wantKind   string
		wantDetail string
	}{
		{
			name:       "patch",
			action:     stagesv1.Action{Patch: &stagesv1.PatchAction{Target: stagesv1.PatchTarget{Kind: "Deployment", Name: "web"}}},
			wantKind:   "patch",
			wantDetail: "Deployment/web",
		},
		{
			name:       "http with method",
			action:     stagesv1.Action{HTTP: &stagesv1.HTTPAction{Method: "PUT", URL: "https://x.test/h"}},
			wantKind:   "http",
			wantDetail: "PUT https://x.test/h",
		},
		{
			name:       "http defaults to POST",
			action:     stagesv1.Action{HTTP: &stagesv1.HTTPAction{URL: "https://x.test/h"}},
			wantKind:   "http",
			wantDetail: "POST https://x.test/h",
		},
		{
			name:       "wait duration",
			action:     stagesv1.Action{Wait: &stagesv1.WaitAction{Duration: dur(30 * time.Second)}},
			wantKind:   "wait",
			wantDetail: "30s",
		},
		{
			name:       "wait expr",
			action:     stagesv1.Action{Wait: &stagesv1.WaitAction{Expr: "status.ready == true"}},
			wantKind:   "wait",
			wantDetail: "expr",
		},
		{
			name:       "wait empty",
			action:     stagesv1.Action{Wait: &stagesv1.WaitAction{}},
			wantKind:   "wait",
			wantDetail: "",
		},
		{
			name:       "job",
			action:     stagesv1.Action{Job: &stagesv1.JobAction{SourceRef: stagesv1.SourceReference{Name: "migrations"}}},
			wantKind:   "job",
			wantDetail: "migrations",
		},
		{
			name:       "delete",
			action:     stagesv1.Action{Delete: &stagesv1.DeleteAction{Target: ref("ConfigMap", "old")}},
			wantKind:   "delete",
			wantDetail: "ConfigMap/old",
		},
		{
			name:       "apply",
			action:     stagesv1.Action{Apply: &stagesv1.ApplyAction{SourceRef: stagesv1.SourceReference{Name: "maintenance"}}},
			wantKind:   "apply",
			wantDetail: "maintenance",
		},
		{
			name:       "nothing set",
			action:     stagesv1.Action{Name: "noop"},
			wantKind:   "action",
			wantDetail: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := tt.action
			kind, detail := describeAction(&a)
			if kind != tt.wantKind || detail != tt.wantDetail {
				t.Errorf("describeAction = (%q,%q), want (%q,%q)", kind, detail, tt.wantKind, tt.wantDetail)
			}
		})
	}
}

// --- stageActionsToRun ---

func TestStageActionsToRun_NoActions(t *testing.T) {
	stage := &stagesv1.Stage{Name: "first"}
	if out := stageActionsToRun(context.Background(), nil, stage, actionplan.VerdictInputs{Revision: "rev1"}); out != nil {
		t.Errorf("stage without actions = %v, want nil", out)
	}
}

func TestStageActionsToRun_EnumeratesAllPhases(t *testing.T) {
	stage := &stagesv1.Stage{
		Name: "first",
		Actions: &stagesv1.StageActions{
			Pre:       []stagesv1.Action{{Name: "p1", Wait: &stagesv1.WaitAction{Expr: "true"}}},
			Post:      []stagesv1.Action{{Name: "p2", HTTP: &stagesv1.HTTPAction{URL: "https://x"}}},
			OnFailure: []stagesv1.Action{{Name: "p3", Delete: &stagesv1.DeleteAction{Target: fluxmeta.NamespacedObjectKindReference{Kind: "Pod", Name: "x"}}}},
		},
	}
	out := stageActionsToRun(context.Background(), nil, stage, actionplan.VerdictInputs{Revision: "rev1"})
	if len(out) != 3 {
		t.Fatalf("want 3 previews, got %d: %+v", len(out), out)
	}
	phases := map[string]string{}
	for _, p := range out {
		phases[p.Name] = p.Phase
		if p.Stage != "first" {
			t.Errorf("preview %q stage = %q, want first", p.Name, p.Stage)
		}
	}
	for name, want := range map[string]string{"p1": "pre", "p2": "post", "p3": "onFailure"} {
		if phases[name] != want {
			t.Errorf("action %q phase = %q, want %q", name, phases[name], want)
		}
	}
}

func TestStageActionsToRun_LedgerOmitsExecuted(t *testing.T) {
	stage := &stagesv1.Stage{
		Name: "first",
		Actions: &stagesv1.StageActions{
			Pre: []stagesv1.Action{
				{Name: "done", Wait: &stagesv1.WaitAction{Expr: "true"}},
				{Name: "todo", Wait: &stagesv1.WaitAction{Expr: "true"}},
			},
		},
	}
	prior := stagesv1.StageStatus{
		LedgerRevision:  "rev1",
		ExecutedActions: []string{"done"},
	}
	out := stageActionsToRun(context.Background(), nil, stage, actionplan.VerdictInputs{Revision: "rev1", Prior: prior})
	if len(out) != 1 || out[0].Name != "todo" {
		t.Fatalf("ledger should omit 'done', got %+v", out)
	}
}

func TestStageActionsToRun_LedgerIgnoredWhenRevisionDiffers(t *testing.T) {
	stage := &stagesv1.Stage{
		Name: "first",
		Actions: &stagesv1.StageActions{
			Pre: []stagesv1.Action{{Name: "done", Wait: &stagesv1.WaitAction{Expr: "true"}}},
		},
	}
	// Ledger pinned to a different revision: every action must be listed.
	prior := stagesv1.StageStatus{LedgerRevision: "other", ExecutedActions: []string{"done"}}
	if out := stageActionsToRun(context.Background(), nil, stage, actionplan.VerdictInputs{Revision: "rev1", Prior: prior}); len(out) != 1 {
		t.Fatalf("mismatched ledger revision should list all, got %+v", out)
	}
}

func TestStageActionsToRun_EmptyRevisionListsAll(t *testing.T) {
	// A local render (no revision) cannot consult the ledger.
	stage := &stagesv1.Stage{
		Name: "first",
		Actions: &stagesv1.StageActions{
			Pre: []stagesv1.Action{{Name: "done", Wait: &stagesv1.WaitAction{Expr: "true"}}},
		},
	}
	prior := stagesv1.StageStatus{LedgerRevision: "", ExecutedActions: []string{"done"}}
	if out := stageActionsToRun(context.Background(), nil, stage, actionplan.VerdictInputs{Prior: prior}); len(out) != 1 {
		t.Fatalf("empty revision should list all, got %+v", out)
	}
}

func TestStageActionsToRun_LifetimeCompletedOmitted(t *testing.T) {
	stage := &stagesv1.Stage{
		Name: "first",
		Actions: &stagesv1.StageActions{
			Post: []stagesv1.Action{
				{Name: "bootstrap", Scope: stagesv1.ScopeLifetime, Wait: &stagesv1.WaitAction{Expr: "true"}},
				{Name: "notify", Wait: &stagesv1.WaitAction{Expr: "true"}},
			},
		},
	}
	// An unanchored Lifetime completion the StageLedger records: a preview at a new
	// revision must omit the once-ever bootstrap (it will not run) but keep notify.
	ledger := &stagesv1.StageLedger{Status: stagesv1.StageLedgerStatus{
		CompletedActions: []stagesv1.LedgerCompletion{{Stage: "first", Action: "bootstrap", Origin: stagesv1.OriginExecuted}},
	}}
	out := stageActionsToRun(context.Background(), nil, stage, actionplan.VerdictInputs{Namespace: "ns", Revision: "rev1", Lifetime: ledger})
	if len(out) != 1 || out[0].Name != "notify" {
		t.Fatalf("a completed Lifetime action must be omitted from the preview; got %+v", out)
	}
}

func TestStageActionsToRun_VersionHeldOmitted(t *testing.T) {
	stage := &stagesv1.Stage{Name: "app", Actions: &stagesv1.StageActions{
		Pre: []stagesv1.Action{
			{Name: "db-upgrade", Scope: stagesv1.ScopeVersion, Wait: &stagesv1.WaitAction{Expr: "true"}},
			{Name: "check", Wait: &stagesv1.WaitAction{Expr: "true"}}, // Revision (default)
		},
	}}
	// A new revision at an unchanged version: the Version action is held (recorded
	// at 2.0.0), the Revision action runs.
	prior := stagesv1.StageStatus{
		LedgerRevision:         "old",
		LedgerVersion:          "2.0.0",
		ExecutedVersionActions: []string{"db-upgrade"},
	}
	in := actionplan.VerdictInputs{Revision: "rev2", Versioned: true, DesiredVersion: "2.0.0", CurrentVersion: "2.0.0", Prior: prior}
	names := map[string]bool{}
	for _, a := range stageActionsToRun(context.Background(), nil, stage, in) {
		names[a.Name] = true
	}
	if names["db-upgrade"] {
		t.Error("a held scope: Version action must be omitted from the diff preview")
	}
	if !names["check"] {
		t.Error("a Revision action at a new revision should be listed")
	}
}

// --- pendingMigrations ---

func TestPendingMigrations_EmptyStatus(t *testing.T) {
	ss := &stagesv1.StageSet{}
	if out := pendingMigrations(ss); out != nil {
		t.Errorf("no pending migrations = %v, want nil", out)
	}
}

func TestPendingMigrations_MapsRichStatus(t *testing.T) {
	// The rich status.pendingMigrations is rendered directly — no spec join — so a
	// sourced ladder (whose content is not in the spec) previews fully too.
	ss := &stagesv1.StageSet{}
	ss.Status.PendingMigrations = []stagesv1.PendingMigration{
		{Name: "m1", To: "v2", From: "v1", Stage: "prod", Actions: []string{"wait", "delete"}, Digest: "abc123"},
	}
	out := pendingMigrations(ss)
	if len(out) != 1 {
		t.Fatalf("want 1 preview, got %d", len(out))
	}
	want := diffrender.MigrationPreview{Name: "m1", To: "v2", From: "v1", Stage: "prod", Actions: []string{"wait", "delete"}}
	if !reflect.DeepEqual(out[0], want) {
		t.Errorf("migration preview = %+v, want %+v", out[0], want)
	}
}

// --- assembleChanges ---

func chg(stage, name string, kind diffrender.ChangeKind) diffrender.Change {
	return diffrender.Change{Stage: stage, Name: name, Kind: kind}
}

func TestAssembleChanges_OrderAndUniqueness(t *testing.T) {
	selected := []stagesv1.Stage{{Name: "first"}, {Name: "second"}}
	diffByStage := map[string][]diffrender.Change{
		"first":  {chg("first", "a", diffrender.ChangeCreate)},
		"second": {chg("second", "b", diffrender.ChangeConfigure)},
	}
	pruneByStage := map[string][]diffrender.Change{
		"first":  {chg("first", "p1", diffrender.ChangeDelete)},
		"second": {chg("second", "p2", diffrender.ChangeDelete)},
		// A prune for a stage no longer in the selected set: emitted last.
		"removed": {chg("removed", "p3", diffrender.ChangeDelete)},
	}

	out := assembleChanges(selected, diffByStage, pruneByStage)

	var order []string
	counts := map[string]int{}
	for _, c := range out {
		order = append(order, c.Name)
		counts[c.Name]++
	}
	// Per spec-stage order: diffs then prunes; removed-stage prunes last.
	want := []string{"a", "p1", "b", "p2", "p3"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Errorf("order = %v, want %v", order, want)
	}
	for name, n := range counts {
		if n != 1 {
			t.Errorf("change %q appears %d times, want exactly once", name, n)
		}
	}
}

func TestAssembleChanges_MultipleRemovedStagesSorted(t *testing.T) {
	selected := []stagesv1.Stage{{Name: "keep"}}
	pruneByStage := map[string][]diffrender.Change{
		"keep":  {chg("keep", "k", diffrender.ChangeDelete)},
		"zebra": {chg("zebra", "z", diffrender.ChangeDelete)},
		"alpha": {chg("alpha", "a", diffrender.ChangeDelete)},
	}
	out := assembleChanges(selected, map[string][]diffrender.Change{}, pruneByStage)
	var order []string
	for _, c := range out {
		order = append(order, c.Name)
	}
	// keep first (selected), then removed stages alphabetically: alpha, zebra.
	want := []string{"k", "a", "z"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Errorf("order = %v, want %v", order, want)
	}
}

// --- diffKind ---

func TestDiffKind_Mapping(t *testing.T) {
	cases := map[apply.DiffAction]diffrender.ChangeKind{
		apply.DiffCreate:          diffrender.ChangeCreate,
		apply.DiffConfigure:       diffrender.ChangeConfigure,
		apply.DiffSkipped:         diffrender.ChangeSkip,
		apply.DiffUnchanged:       diffrender.ChangeUnchanged,
		apply.DiffAction("bogus"): diffrender.ChangeUnchanged,
	}
	for in, want := range cases {
		if got := diffKind(in); got != want {
			t.Errorf("diffKind(%q) = %q, want %q", in, got, want)
		}
	}
}

// --- hasSpecStage ---

func TestHasSpecStage(t *testing.T) {
	ss := &stagesv1.StageSet{
		Spec: stagesv1.StageSetSpec{
			Stages: []stagesv1.Stage{{Name: "canary"}, {Name: "prod"}},
		},
	}
	if !hasSpecStage(ss, "prod") {
		t.Error("prod should be found")
	}
	if hasSpecStage(ss, "ghost") {
		t.Error("ghost should not be found")
	}
	if hasSpecStage(&stagesv1.StageSet{}, "anything") {
		t.Error("empty spec should find nothing")
	}
}

// --- sourceGVK ---

func TestSourceGVK(t *testing.T) {
	t.Run("default kind is ExternalArtifact", func(t *testing.T) {
		if got := sourceGVK(stagesv1.SourceReference{Name: "art"}); got != artifact.ExternalArtifactGVK {
			t.Errorf("empty kind = %v, want %v", got, artifact.ExternalArtifactGVK)
		}
	})
	t.Run("explicit ExternalArtifact", func(t *testing.T) {
		ref := stagesv1.SourceReference{Kind: "ExternalArtifact", Name: "art"}
		if got := sourceGVK(ref); got != artifact.ExternalArtifactGVK {
			t.Errorf("explicit EA = %v, want %v", got, artifact.ExternalArtifactGVK)
		}
	})
	t.Run("producer ref uses its own GVK", func(t *testing.T) {
		ref := stagesv1.SourceReference{
			APIVersion: "jaas.metio.wtf/v1",
			Kind:       "JsonnetSnippet",
			Name:       "snip",
		}
		got := sourceGVK(ref)
		if got.Group != "jaas.metio.wtf" || got.Version != "v1" || got.Kind != "JsonnetSnippet" {
			t.Errorf("producer ref GVK = %v", got)
		}
	})
	t.Run("producer ref with bare apiVersion (no group)", func(t *testing.T) {
		ref := stagesv1.SourceReference{APIVersion: "v1", Kind: "Foo", Name: "x"}
		got := sourceGVK(ref)
		if got.Group != "" || got.Version != "v1" || got.Kind != "Foo" {
			t.Errorf("bare apiVersion GVK = %v", got)
		}
	})
}

// --- newToken ---

func TestNewToken_FormatAndUniqueness(t *testing.T) {
	tok := newToken()
	if _, err := time.Parse(time.RFC3339Nano, tok); err != nil {
		t.Fatalf("token %q not RFC3339Nano: %v", tok, err)
	}
	// Two successive tokens must differ. RFC3339Nano keeps nanosecond
	// resolution, but to avoid any clock-granularity flakiness, retry briefly.
	prev := tok
	for range 1000 {
		next := newToken()
		if next != prev {
			return
		}
		prev = next
		time.Sleep(time.Microsecond)
	}
	t.Fatal("newToken never changed across 1000 calls")
}

// --- fuzz: parseSourceDirs never panics and obeys its contract ---

func FuzzParseSourceDirs(f *testing.F) {
	seeds := [][]string{
		nil,
		{},
		{"/all"},
		{"canary=/c"},
		{"a=/x", "b=/y"},
		{"a=/x", "a=/y"},
		{""},
		{"="},
		{"stage="},
		{"=/path"},
		{"a=b=c"},
		{"  =  "},
		{"\x00"},
	}
	for _, s := range seeds {
		// Fuzz drives at most three entries, joined by NUL into one corpus arg.
		f.Add(strings.Join(s, "\x00"))
	}

	f.Fuzz(func(t *testing.T, joined string) {
		var entries []string
		if joined != "" {
			entries = strings.Split(joined, "\x00")
		}

		out, err := parseSourceDirs(entries)

		if len(entries) == 0 {
			if out != nil || err != nil {
				t.Fatalf("empty input = %v,%v, want nil,nil", out, err)
			}
			return
		}

		if err != nil {
			// On error the map must not be returned for use.
			if out != nil {
				t.Fatalf("error path returned non-nil map: %v", out)
			}
			return
		}

		// Success path invariants: every parsed key maps to a non-empty path,
		// and the key/value match the STAGE=PATH split of some input entry.
		for k, v := range out {
			if v == "" {
				t.Fatalf("key %q mapped to empty path", k)
			}
		}
		// No duplicate keys could have survived (map enforces uniqueness, and
		// the function rejects duplicates), so |out| <= |entries|.
		if len(out) > len(entries) {
			t.Fatalf("output has more keys (%d) than entries (%d)", len(out), len(entries))
		}
	})
}

// FuzzColorEnabled exercises the mode parser against arbitrary strings: it must
// never panic, and must return an error for exactly the modes outside the
// allowed set.
func FuzzColorEnabled(f *testing.F) {
	for _, s := range []string{"auto", "always", "never", "", "AUTO", "Always", "x", "  "} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, mode string) {
		t.Setenv("NO_COLOR", "")
		got, err := colorEnabled(mode, &bytes.Buffer{})
		valid := mode == "always" || mode == "never" || mode == "" || mode == "auto"
		if valid == (err != nil) {
			t.Fatalf("mode %q: valid=%v but err=%v", mode, valid, err)
		}
		// Only "always" forces color on; every other resolution against a
		// non-*os.File writer (auto/empty) or an error path is off.
		if got && mode != "always" {
			t.Fatalf("mode %q: color enabled against non-terminal writer", mode)
		}
	})
}
