// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package diffrender

import (
	"bytes"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// --- StripNoise edge cases ---

func TestStripNoise_NilIsSafeNoop(t *testing.T) {
	t.Parallel()
	// A nil object must not panic.
	StripNoise(nil)
}

func TestStripNoise_NoMetadataDoesNotPanic(t *testing.T) {
	t.Parallel()
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"data":       map[string]any{"k": "v"},
	}}
	StripNoise(obj)
	if _, present := obj.Object["data"]; !present {
		t.Error("non-metadata field wrongly removed")
	}
	if _, present := obj.Object["metadata"]; present {
		t.Error("metadata key materialized where none existed")
	}
}

func TestStripNoise_StripsSelfLink(t *testing.T) {
	t.Parallel()
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{
			"name":     "cfg",
			"selfLink": "/api/v1/namespaces/ns/configmaps/cfg",
		},
	}}
	StripNoise(obj)
	md, _, _ := unstructured.NestedMap(obj.Object, "metadata")
	if _, present := md["selfLink"]; present {
		t.Error("metadata.selfLink not stripped")
	}
}

func TestStripNoise_KeepsOtherAnnotationsAndOmitsEmptyMap(t *testing.T) {
	t.Parallel()
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{
			"name": "cfg",
			"annotations": map[string]any{
				"kubectl.kubernetes.io/last-applied-configuration": "{}",
				"team": "platform",
			},
		},
	}}
	StripNoise(obj)
	ann, found, _ := unstructured.NestedStringMap(obj.Object, "metadata", "annotations")
	if !found {
		t.Fatal("annotations dropped even though a real annotation remains")
	}
	if ann["team"] != "platform" {
		t.Errorf("real annotation lost: %v", ann)
	}
	if _, present := ann["kubectl.kubernetes.io/last-applied-configuration"]; present {
		t.Error("last-applied annotation not stripped")
	}
}

func TestStripNoise_DroppedEmptyAnnotationsNotInYAML(t *testing.T) {
	t.Parallel()
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{
			"name":        "cfg",
			"annotations": map[string]any{"kubectl.kubernetes.io/last-applied-configuration": "{}"},
		},
	}}
	StripNoise(obj)
	out, err := ToYAML(obj)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "annotations:") {
		t.Errorf("emptied annotations rendered into YAML:\n%s", out)
	}
}

// --- renderSide ---

func TestRenderSide_NilRendersEmpty(t *testing.T) {
	t.Parallel()
	got, err := renderSide(nil, NewSecretMasker(false))
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("nil object should render empty, got %q", got)
	}
}

func TestRenderSide_StripsNoiseAndMasks(t *testing.T) {
	t.Parallel()
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]any{
			"name":            "creds",
			"resourceVersion": "999",
			"uid":             "deadbeef",
		},
		"data":   map[string]any{"password": "c2VjcmV0"},
		"status": map[string]any{"phase": "Active"},
	}}
	got, err := renderSide(obj, NewSecretMasker(false))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "c2VjcmV0") {
		t.Errorf("secret plaintext leaked through renderSide:\n%s", got)
	}
	if !strings.Contains(got, "value not shown") {
		t.Errorf("mask placeholder missing:\n%s", got)
	}
	for _, noise := range []string{"resourceVersion", "deadbeef", "status:"} {
		if strings.Contains(got, noise) {
			t.Errorf("noise %q not stripped:\n%s", noise, got)
		}
	}
}

func TestRenderSide_DoesNotMutateInput(t *testing.T) {
	t.Parallel()
	// renderSide must work on a copy: the caller's live object keeps its noise
	// and plaintext so a second render (the other diff side) is unaffected.
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]any{"name": "creds", "resourceVersion": "7"},
		"data":     map[string]any{"k": "cGxhaW4="},
	}}
	orig := obj.DeepCopy()
	if _, err := renderSide(obj, NewSecretMasker(false)); err != nil {
		t.Fatal(err)
	}
	if rv, _, _ := unstructured.NestedString(obj.Object, "metadata", "resourceVersion"); rv != "7" {
		t.Error("renderSide mutated caller's metadata")
	}
	if !equalObjects(obj, orig) {
		t.Error("renderSide mutated caller's object")
	}
}

func equalObjects(a, b *unstructured.Unstructured) bool {
	ay, _ := ToYAML(a)
	by, _ := ToYAML(b)
	return bytes.Equal(ay, by)
}

// --- unifiedBody ---

func TestUnifiedBody_IdenticalIsEmpty(t *testing.T) {
	t.Parallel()
	got, err := unifiedBody("same\ncontent\n", "same\ncontent\n")
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("identical sides should yield no diff, got %q", got)
	}
}

func TestUnifiedBody_DiffersHasHeadersAndContext(t *testing.T) {
	t.Parallel()
	before := "a\nb\nc\nd\ne\nf\ng\n"
	after := "a\nb\nc\nD\ne\nf\ng\n"
	got, err := unifiedBody(before, after)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "--- live") {
		t.Errorf("missing --- live header:\n%s", got)
	}
	if !strings.Contains(got, "+++ merged") {
		t.Errorf("missing +++ merged header:\n%s", got)
	}
	if !strings.Contains(got, "-d") || !strings.Contains(got, "+D") {
		t.Errorf("missing changed lines:\n%s", got)
	}
	// 3 lines of context on each side of the single change: a, b, c above; e, f, g below.
	for _, ctx := range []string{" a", " b", " c", " e", " f", " g"} {
		if !strings.Contains(got, ctx) {
			t.Errorf("expected 3 lines of context, missing %q:\n%s", ctx, got)
		}
	}
}

// --- summaryLine: every single-kind branch + mixtures ---

func TestSummaryLine_AllBranches(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		sum  Summary
		want string
	}{
		{"only-create", Summary{Create: 1}, "Summary: 1 to create"},
		{"only-configure", Summary{Configure: 2}, "Summary: 2 to configure"},
		{"only-delete", Summary{Delete: 3}, "Summary: 3 to delete"},
		{"only-unchanged", Summary{Unchanged: 4}, "Summary: 4 unchanged"},
		{"only-skip", Summary{Skip: 5}, "Summary: 5 skipped"},
		{"all-zero", Summary{}, "Summary: no changes"},
		{
			"full-mixture",
			Summary{Create: 1, Configure: 2, Delete: 3, Unchanged: 4, Skip: 5},
			"Summary: 1 to create, 2 to configure, 3 to delete, 4 unchanged, 5 skipped",
		},
		{
			"create-and-skip",
			Summary{Create: 1, Skip: 2},
			"Summary: 1 to create, 2 skipped",
		},
		{
			"configure-and-delete-order",
			Summary{Delete: 1, Configure: 1},
			"Summary: 1 to configure, 1 to delete",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := summaryLine(tc.sum); got != tc.want {
				t.Errorf("summaryLine(%+v) = %q, want %q", tc.sum, got, tc.want)
			}
		})
	}
}

func TestSummary_Changed_AllCombinations(t *testing.T) {
	t.Parallel()
	cases := []struct {
		sum  Summary
		want bool
	}{
		{Summary{}, false},
		{Summary{Unchanged: 9}, false},
		{Summary{Skip: 9}, false},
		{Summary{Unchanged: 3, Skip: 4}, false},
		{Summary{Create: 1}, true},
		{Summary{Configure: 1}, true},
		{Summary{Delete: 1}, true},
		{Summary{Create: 1, Unchanged: 5, Skip: 5}, true},
	}
	for _, tc := range cases {
		if got := tc.sum.Changed(); got != tc.want {
			t.Errorf("(%+v).Changed() = %v, want %v", tc.sum, got, tc.want)
		}
	}
}

// --- header ---

func TestHeader_NamespaceAndStage(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		ch   Change
		want []string
		not  []string
	}{
		{
			name: "no-namespace-no-stage",
			ch:   Change{Kind: ChangeCreate, GVK: gvk("ClusterRole"), Name: "admin"},
			want: []string{"create ClusterRole/admin"},
			not:  []string{"[", "stage:"},
		},
		{
			name: "namespace-only",
			ch:   Change{Kind: ChangeConfigure, GVK: gvk("ConfigMap"), Namespace: "prod", Name: "cfg"},
			want: []string{"configure ConfigMap/cfg [prod]"},
			not:  []string{"stage:"},
		},
		{
			name: "namespace-and-stage",
			ch:   Change{Kind: ChangeDelete, GVK: gvk("Secret"), Namespace: "ns", Name: "old", Stage: "canary"},
			want: []string{"delete Secret/old [ns]", "(stage: canary)"},
		},
		{
			name: "stage-without-namespace",
			ch:   Change{Kind: ChangeCreate, GVK: gvk("ClusterRole"), Name: "admin", Stage: "prod"},
			want: []string{"create ClusterRole/admin", "(stage: prod)"},
			not:  []string{"[admin]", "[]"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := header(tc.ch, false)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("header = %q, missing %q", got, w)
				}
			}
			for _, n := range tc.not {
				if strings.Contains(got, n) {
					t.Errorf("header = %q, should not contain %q", got, n)
				}
			}
			if strings.Contains(got, "\x1b[") {
				t.Errorf("color-off header has ANSI: %q", got)
			}
		})
	}
}

// --- colorFor / header colors ---

func TestColorFor(t *testing.T) {
	t.Parallel()
	cases := []struct {
		kind ChangeKind
		want string
	}{
		{ChangeCreate, ansiGreen},
		{ChangeDelete, ansiRed},
		{ChangeConfigure, ansiCyan},
		{ChangeUnchanged, ""},
		{ChangeSkip, ""},
		{ChangeKind("bogus"), ""},
	}
	for _, tc := range cases {
		if got := colorFor(tc.kind); got != tc.want {
			t.Errorf("colorFor(%q) = %q, want %q", tc.kind, got, tc.want)
		}
	}
}

func TestHeader_ColorPerKind(t *testing.T) {
	t.Parallel()
	cases := []struct {
		kind   ChangeKind
		prefix string
	}{
		{ChangeCreate, ansiGreen},
		{ChangeDelete, ansiRed},
		{ChangeConfigure, ansiCyan},
	}
	for _, tc := range cases {
		ch := Change{Kind: tc.kind, GVK: gvk("ConfigMap"), Name: "x"}
		got := header(ch, true)
		if !strings.HasPrefix(got, tc.prefix) {
			t.Errorf("header(%q, color) = %q, want prefix %q", tc.kind, got, tc.prefix)
		}
		if !strings.HasSuffix(got, ansiReset) {
			t.Errorf("header(%q, color) = %q, want suffix reset", tc.kind, got)
		}
		// Stripping ANSI recovers the uncolored header.
		if stripANSI(got) != header(ch, false) {
			t.Errorf("colored header not additive for %q: %q", tc.kind, got)
		}
	}
}

func TestHeader_ColorNeutralKindsNoTint(t *testing.T) {
	t.Parallel()
	// unchanged/skip have no color; their colored header still carries a reset
	// suffix but no leading SGR color, so it equals the plain label + reset.
	for _, kind := range []ChangeKind{ChangeUnchanged, ChangeSkip} {
		ch := Change{Kind: kind, GVK: gvk("ConfigMap"), Name: "x"}
		got := header(ch, true)
		if strings.HasPrefix(got, ansiGreen) || strings.HasPrefix(got, ansiRed) || strings.HasPrefix(got, ansiCyan) {
			t.Errorf("neutral kind %q got a color prefix: %q", kind, got)
		}
		if stripANSI(got) != header(ch, false) {
			t.Errorf("neutral colored header not additive for %q: %q", kind, got)
		}
	}
}

// --- colorize specifics: gutter tinting and untinted file headers ---

func TestColorize_TintsGuttersNotFileHeaders(t *testing.T) {
	t.Parallel()
	body := "--- live\n+++ merged\n@@ -1,2 +1,2 @@\n context\n-old\n+new\n"
	got := colorize(body, true)

	// --- and +++ lines must be emitted verbatim (no color), even though they
	// begin with - / + prefixes.
	if !strings.Contains(got, "--- live\n") {
		t.Errorf("--- live tinted or altered:\n%q", got)
	}
	if !strings.Contains(got, "+++ merged\n") {
		t.Errorf("+++ merged tinted or altered:\n%q", got)
	}
	if !strings.Contains(got, ansiGreen+"+new"+ansiReset) {
		t.Errorf("addition not green:\n%q", got)
	}
	if !strings.Contains(got, ansiRed+"-old"+ansiReset) {
		t.Errorf("removal not red:\n%q", got)
	}
	if !strings.Contains(got, ansiCyan+"@@ -1,2 +1,2 @@"+ansiReset) {
		t.Errorf("hunk header not cyan:\n%q", got)
	}
}

func TestColorize_RoundTripEqualsUncolored(t *testing.T) {
	t.Parallel()
	body := "--- live\n+++ merged\n@@ -1,3 +1,3 @@\n keep\n-drop\n+add\n trailing"
	if stripANSI(colorize(body, true)) != colorize(body, false) {
		t.Error("strip(colorize(true)) != colorize(false)")
	}
}

// --- RenderDiff: skip visibility, grouping order, full multi-change render ---

func TestRenderDiff_SkipHiddenByDefault(t *testing.T) {
	t.Parallel()
	changes := []Change{{Kind: ChangeSkip, GVK: gvk("ConfigMap"), Name: "ignored"}}

	out, sum := render(t, changes, RenderOptions{})
	if sum.Skip != 1 || sum.Changed() {
		t.Fatalf("summary = %+v", sum)
	}
	if strings.Contains(out, "ignored") {
		t.Errorf("skip object shown without --show-unchanged:\n%s", out)
	}

	out, _ = render(t, changes, RenderOptions{ShowUnchanged: true})
	if !strings.Contains(out, "skip ConfigMap/ignored") {
		t.Errorf("skip object not shown with --show-unchanged:\n%s", out)
	}
}

func TestRenderDiff_GroupingIsInputOrder(t *testing.T) {
	t.Parallel()
	changes := []Change{
		{Kind: ChangeCreate, GVK: gvk("ConfigMap"), Name: "first", After: cm("first", map[string]any{"k": "v"})},
		{Kind: ChangeDelete, GVK: gvk("ConfigMap"), Name: "second", Before: cm("second", map[string]any{"k": "v"})},
		{
			Kind: ChangeConfigure, GVK: gvk("ConfigMap"), Name: "third",
			Before: cm("third", map[string]any{"k": "1"}), After: cm("third", map[string]any{"k": "2"}),
		},
	}
	out, sum := render(t, changes, RenderOptions{})
	if sum.Create != 1 || sum.Delete != 1 || sum.Configure != 1 {
		t.Fatalf("summary = %+v", sum)
	}
	iFirst := strings.Index(out, "create ConfigMap/first")
	iSecond := strings.Index(out, "delete ConfigMap/second")
	iThird := strings.Index(out, "configure ConfigMap/third")
	if iFirst < 0 || iSecond < 0 || iThird < 0 {
		t.Fatalf("missing a header:\n%s", out)
	}
	if !(iFirst < iSecond && iSecond < iThird) {
		t.Errorf("headers not in input order: first=%d second=%d third=%d\n%s", iFirst, iSecond, iThird, out)
	}
}

func TestRenderDiff_CountsAccumulateAcrossMixedKinds(t *testing.T) {
	t.Parallel()
	changes := []Change{
		{Kind: ChangeCreate, GVK: gvk("ConfigMap"), Name: "a", After: cm("a", map[string]any{"k": "v"})},
		{Kind: ChangeCreate, GVK: gvk("ConfigMap"), Name: "b", After: cm("b", map[string]any{"k": "v"})},
		{
			Kind: ChangeConfigure, GVK: gvk("ConfigMap"), Name: "c",
			Before: cm("c", map[string]any{"k": "1"}), After: cm("c", map[string]any{"k": "2"}),
		},
		{Kind: ChangeDelete, GVK: gvk("ConfigMap"), Name: "d", Before: cm("d", map[string]any{"k": "v"})},
		{Kind: ChangeUnchanged, GVK: gvk("ConfigMap"), Name: "e"},
		{Kind: ChangeUnchanged, GVK: gvk("ConfigMap"), Name: "f"},
		{Kind: ChangeUnchanged, GVK: gvk("ConfigMap"), Name: "g"},
		{Kind: ChangeSkip, GVK: gvk("ConfigMap"), Name: "h"},
	}
	_, sum := render(t, changes, RenderOptions{})
	want := Summary{Create: 2, Configure: 1, Delete: 1, Unchanged: 3, Skip: 1}
	if sum != want {
		t.Errorf("summary = %+v, want %+v", sum, want)
	}
}

func TestRenderDiff_CreateShowsOnlyAdditions(t *testing.T) {
	t.Parallel()
	out, _ := render(t, []Change{{
		Kind: ChangeCreate, GVK: gvk("ConfigMap"), Name: "new", After: cm("new", map[string]any{"k": "v"}),
	}}, RenderOptions{})
	// A create has an empty "before" side, so the body must add lines, not remove.
	if !strings.Contains(out, "+apiVersion: v1") {
		t.Errorf("create body has no added lines:\n%s", out)
	}
	// No removal gutter lines (a single "-", not the "---" file header).
	for line := range strings.SplitSeq(out, "\n") {
		if strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---") {
			t.Errorf("create body has a removal line %q:\n%s", line, out)
		}
	}
}

func TestRenderDiff_DefaultMaskerMasksSecrets(t *testing.T) {
	t.Parallel()
	// Masker left nil in RenderOptions must default to a masking (non-reveal) masker.
	out, _ := render(t, []Change{{
		Kind: ChangeCreate, GVK: gvk("Secret"), Name: "creds",
		After: secret(map[string]any{"password": "c2VjcmV0"}),
	}}, RenderOptions{})
	if strings.Contains(out, "c2VjcmV0") {
		t.Errorf("default masker did not mask secret:\n%s", out)
	}
}

func TestRenderDiff_RevealMaskerShowsSecrets(t *testing.T) {
	t.Parallel()
	out, _ := render(t, []Change{{
		Kind: ChangeCreate, GVK: gvk("Secret"), Name: "creds",
		After: secret(map[string]any{"password": "c2VjcmV0"}),
	}}, RenderOptions{Masker: NewSecretMasker(true)})
	if !strings.Contains(out, "c2VjcmV0") {
		t.Errorf("reveal masker should expose secret value:\n%s", out)
	}
}

func TestRenderDiff_SecretMaskedOnBothSides(t *testing.T) {
	t.Parallel()
	before := secret(map[string]any{"password": "b2xk"})
	after := secret(map[string]any{"password": "bmV3"})
	out, _ := render(t, []Change{{
		Kind: ChangeConfigure, GVK: gvk("Secret"), Name: "creds", Before: before, After: after,
	}}, RenderOptions{})
	if strings.Contains(out, "b2xk") {
		t.Errorf("before-side secret leaked:\n%s", out)
	}
	if strings.Contains(out, "bmV3") {
		t.Errorf("after-side secret leaked:\n%s", out)
	}
	// Differing values must surface as a change in the diff (distinct placeholders).
	if !strings.Contains(out, "-") || !strings.Contains(out, "+") {
		t.Errorf("changed secret value did not surface as a diff:\n%s", out)
	}
}

// --- isSecret via Mask: kind and apiVersion gating ---

func TestMask_GatesOnKindAndAPIVersion(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		apiVersion string
		kind       string
		wantMasked bool
	}{
		{"core-v1-secret", "v1", "Secret", true},
		{"empty-apiversion-secret", "", "Secret", true},
		{"wrong-apiversion-secret", "bitnami.com/v1alpha1", "Secret", false},
		{"non-secret-kind", "v1", "ConfigMap", false},
		{"sealed-secret", "bitnami.com/v1alpha1", "SealedSecret", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			obj := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": tc.apiVersion, "kind": tc.kind,
				"metadata": map[string]any{"name": "x"},
				"data":     map[string]any{"k": "plain"},
			}}
			NewSecretMasker(false).Mask(obj)
			val, _, _ := unstructured.NestedString(obj.Object, "data", "k")
			masked := val != "plain"
			if masked != tc.wantMasked {
				t.Errorf("masked=%v, want %v (value=%q)", masked, tc.wantMasked, val)
			}
		})
	}
}

func TestMask_NilMaskerAndNilObject(t *testing.T) {
	t.Parallel()
	// A nil *SecretMasker is a no-op and must not panic.
	var m *SecretMasker
	m.Mask(secret(map[string]any{"k": "QQ=="}))

	// A non-nil masker handed a nil object is a no-op and must not panic.
	NewSecretMasker(false).Mask(nil)
}

func TestMask_BothDataAndStringData(t *testing.T) {
	t.Parallel()
	s := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata":   map[string]any{"name": "creds"},
		"data":       map[string]any{"a": "QQ=="},
		"stringData": map[string]any{"b": "plaintext"},
	}}
	NewSecretMasker(false).Mask(s)
	d, _, _ := unstructured.NestedStringMap(s.Object, "data")
	sd, _, _ := unstructured.NestedStringMap(s.Object, "stringData")
	if strings.Contains(d["a"], "QQ==") || !strings.HasPrefix(d["a"], "<-- value not shown") {
		t.Errorf("data not masked: %q", d["a"])
	}
	if strings.Contains(sd["b"], "plaintext") || !strings.HasPrefix(sd["b"], "<-- value not shown") {
		t.Errorf("stringData not masked: %q", sd["b"])
	}
}

func TestMaskPlaceholder_ExactString(t *testing.T) {
	t.Parallel()
	if got := maskPlaceholder(1); got != "<-- value not shown (#1)" {
		t.Errorf("placeholder #1 = %q", got)
	}
	if got := maskPlaceholder(42); got != "<-- value not shown (#42)" {
		t.Errorf("placeholder #42 = %q", got)
	}
}

// --- ToYAML determinism: key order must not produce spurious diffs ---

func TestToYAML_DeterministicSortedKeys(t *testing.T) {
	t.Parallel()
	a := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{
			"name": "cfg",
			"labels": map[string]any{
				"app": "web", "tier": "frontend", "env": "prod", "zone": "a",
			},
		},
	}}
	b := &unstructured.Unstructured{Object: map[string]any{
		"kind": "ConfigMap", "apiVersion": "v1",
		"metadata": map[string]any{
			"labels": map[string]any{
				"zone": "a", "env": "prod", "tier": "frontend", "app": "web",
			},
			"name": "cfg",
		},
	}}
	ay, err := ToYAML(a)
	if err != nil {
		t.Fatal(err)
	}
	by, err := ToYAML(b)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ay, by) {
		t.Errorf("YAML differs for same content inserted in different order:\n%s\n---\n%s", ay, by)
	}
}

func TestToYAML_ErrorsOnUnmarshalableValue(t *testing.T) {
	t.Parallel()
	// ToYAML marshals via JSON; a value JSON cannot encode surfaces an error
	// rather than panicking or producing partial output. This is the only input
	// shape that reaches ToYAML's error return — the higher-level helpers
	// (renderSide, RenderManifests) DeepCopy first, which rejects such values
	// before marshal, so their marshal-error propagation is defensive.
	obj := &unstructured.Unstructured{Object: map[string]any{"k": make(chan int)}}
	if _, err := ToYAML(obj); err == nil {
		t.Fatal("expected error marshaling an unmarshalable value")
	}
}

func TestToYAML_StableAcrossInvocations(t *testing.T) {
	t.Parallel()
	obj := cm("cfg", map[string]any{"z": "1", "a": "2", "m": "3"})
	first, err := ToYAML(obj)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 5 {
		again, err := ToYAML(obj)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(first, again) {
			t.Fatalf("ToYAML not stable on invocation %d", i)
		}
	}
}

func TestRenderDiff_LabelOrderNotASpuriousDiff(t *testing.T) {
	t.Parallel()
	before := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{
			"name":   "cfg",
			"labels": map[string]any{"a": "1", "b": "2", "c": "3"},
		},
	}}
	after := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{
			"name":   "cfg",
			"labels": map[string]any{"c": "3", "b": "2", "a": "1"},
		},
	}}
	out, _ := render(t, []Change{{
		Kind: ChangeConfigure, GVK: gvk("ConfigMap"), Name: "cfg", Before: before, After: after,
	}}, RenderOptions{})
	// Same labels, different insertion order: the diff body must be empty.
	if strings.Contains(out, "@@") {
		t.Errorf("label reordering produced a spurious diff:\n%s", out)
	}
}

// --- RenderManifests: single object has no leading separator ---

func TestRenderManifests_SingleObjectNoSeparator(t *testing.T) {
	t.Parallel()
	out, err := RenderManifests([]*unstructured.Unstructured{
		cm("only", map[string]any{"k": "v"}),
	}, NewSecretMasker(false))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "---") {
		t.Errorf("single object should have no separator:\n%s", out)
	}
	if !strings.Contains(out, "name: only") {
		t.Errorf("object not rendered:\n%s", out)
	}
}

func TestRenderManifests_ExactSeparatorCount(t *testing.T) {
	t.Parallel()
	objs := []*unstructured.Unstructured{
		cm("a", map[string]any{"k": "v"}),
		cm("b", map[string]any{"k": "v"}),
		cm("c", map[string]any{"k": "v"}),
	}
	out, err := RenderManifests(objs, NewSecretMasker(false))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(out, "---\n"); got != 2 {
		t.Errorf("3 objects should have 2 separators, got %d:\n%s", got, out)
	}
}

func TestRenderManifests_RevealMaskerShowsSecret(t *testing.T) {
	t.Parallel()
	out, err := RenderManifests([]*unstructured.Unstructured{
		secret(map[string]any{"p": "c2VjcmV0"}),
	}, NewSecretMasker(true))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "c2VjcmV0") {
		t.Errorf("reveal masker should expose value:\n%s", out)
	}
}

func TestRenderManifests_DoesNotMutateInput(t *testing.T) {
	t.Parallel()
	s := secret(map[string]any{"p": "c2VjcmV0"})
	orig := s.DeepCopy()
	if _, err := RenderManifests([]*unstructured.Unstructured{s}, NewSecretMasker(false)); err != nil {
		t.Fatal(err)
	}
	if !equalObjects(s, orig) {
		t.Error("RenderManifests mutated caller's object")
	}
}

// --- headingText colored branch (preview.go) ---

func TestHeadingText_ColorWrapsInCyan(t *testing.T) {
	t.Parallel()
	if got := headingText("Actions to run:", false); got != "Actions to run:" {
		t.Errorf("no-color heading altered: %q", got)
	}
	got := headingText("Actions to run:", true)
	if !strings.HasPrefix(got, ansiCyan) || !strings.HasSuffix(got, ansiReset) {
		t.Errorf("colored heading not cyan-wrapped: %q", got)
	}
	if stripANSI(got) != "Actions to run:" {
		t.Errorf("colored heading not additive: %q", got)
	}
}
