// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package diffrender

import (
	"bytes"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// ansiPattern matches the SGR escape sequences colorize emits (CSI … m). It is
// used to prove color is purely additive: stripping it from a colored render
// must yield the uncolored render byte-for-byte.
var ansiPattern = regexp.MustCompile("\x1b\\[[0-9;]*m")

func stripANSI(s string) string {
	return ansiPattern.ReplaceAllString(s, "")
}

// secretWith builds a Secret unstructured with a single data value, suitable
// for masking assertions.
func secretWith(value string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata":   map[string]any{"name": "creds"},
		"data":       map[string]any{"k": value},
	}}
}

// FuzzMask hardens the core security property of SecretMasker: a masked Secret
// data value is replaced wholesale by the fixed placeholder template, no matter
// what bytes the value carries. The robust statement of "plaintext never leaks"
// is structural — the masked field must EQUAL a placeholder — rather than a
// substring check, because the placeholder template itself contains common
// characters that an arbitrary value could coincidentally share.
func FuzzMask(f *testing.F) {
	for _, seed := range []string{"hunter2", "QQ==", "c2VjcmV0", "", " ", "\n", "value not shown", "#1", "<-- value not shown (#1)"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		s := secretWith(value)
		NewSecretMasker(false).Mask(s)

		data, _, err := unstructured.NestedStringMap(s.Object, "data")
		if err != nil {
			t.Fatalf("data not a string map after mask: %v", err)
		}
		if data["k"] != maskPlaceholder(1) {
			t.Fatalf("masked value is not a clean placeholder: %q", data["k"])
		}

		// Masking an already-masked Secret still produces a clean placeholder —
		// the security property holds across a second pass.
		again := s.DeepCopy()
		NewSecretMasker(false).Mask(again)
		d2, _, _ := unstructured.NestedStringMap(again.Object, "data")
		if d2["k"] != maskPlaceholder(1) {
			t.Fatalf("second mask pass produced %q, not a clean placeholder", d2["k"])
		}
	})
}

// FuzzMaskPlaceholderDistinctness pins the placeholder contract within one
// masker: equal values collapse to one placeholder, distinct values get
// distinct placeholders.
func FuzzMaskPlaceholderDistinctness(f *testing.F) {
	f.Add("alpha", "beta")
	f.Add("same", "same")
	f.Add("", "x")
	f.Add("\n", "\t")
	f.Fuzz(func(t *testing.T, a, b string) {
		s := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "Secret",
			"metadata":   map[string]any{"name": "creds"},
			"data":       map[string]any{"x": a, "y": a, "z": b},
		}}
		NewSecretMasker(false).Mask(s)
		data, _, _ := unstructured.NestedStringMap(s.Object, "data")

		if data["x"] != data["y"] {
			t.Fatalf("identical values produced different placeholders: %q vs %q", data["x"], data["y"])
		}
		if a == b {
			if data["x"] != data["z"] {
				t.Fatalf("equal values across keys differ: %q vs %q", data["x"], data["z"])
			}
		} else if data["x"] == data["z"] {
			t.Fatalf("distinct values %q/%q share placeholder %q", a, b, data["x"])
		}
	})
}

// FuzzMaskNonStringData feeds a Secret whose data value is not a string. Mask
// stringifies via fmt and must still produce a placeholder without panicking or
// leaking. The value is fuzzed as an int64 to exercise the non-string path.
func FuzzMaskNonStringData(f *testing.F) {
	f.Add(int64(0))
	f.Add(int64(-1))
	f.Add(int64(1 << 40))
	f.Fuzz(func(t *testing.T, n int64) {
		s := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "Secret",
			"metadata":   map[string]any{"name": "creds"},
			"data":       map[string]any{"n": n},
		}}
		NewSecretMasker(false).Mask(s)
		data, _, err := unstructured.NestedStringMap(s.Object, "data")
		if err != nil {
			t.Fatalf("data not a string map after masking non-string value: %v", err)
		}
		if !strings.HasPrefix(data["n"], "<-- value not shown") {
			t.Fatalf("non-string value not masked: %q", data["n"])
		}
	})
}

// FuzzStripNoise feeds arbitrary content alongside the noise fields and asserts
// StripNoise never panics, always removes status and metadata.managedFields,
// and is idempotent.
func FuzzStripNoise(f *testing.F) {
	f.Add("k1", "v1", "status-val")
	f.Add("", "", "")
	f.Add("annoKey", "annoVal", "deep")
	f.Fuzz(func(t *testing.T, key, val, statusVal string) {
		obj := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"metadata": map[string]any{
				"name":          "web",
				"managedFields": []any{map[string]any{"manager": "x"}},
				"annotations": map[string]any{
					"kubectl.kubernetes.io/last-applied-configuration": "{...}",
					key: val,
				},
			},
			"spec":   map[string]any{key: val},
			"status": map[string]any{"phase": statusVal},
		}}

		StripNoise(obj)

		if _, present := obj.Object["status"]; present {
			t.Fatal("status not stripped")
		}
		if md, found, _ := unstructured.NestedMap(obj.Object, "metadata"); found {
			if _, present := md["managedFields"]; present {
				t.Fatal("metadata.managedFields not stripped")
			}
		}
		ann, _, _ := unstructured.NestedMap(obj.Object, "metadata", "annotations")
		if _, present := ann["kubectl.kubernetes.io/last-applied-configuration"]; present {
			t.Fatal("last-applied annotation not stripped")
		}

		// Idempotence: a second strip is a no-op.
		once := obj.DeepCopy()
		StripNoise(obj)
		if !reflect.DeepEqual(once.Object, obj.Object) {
			t.Fatalf("StripNoise not idempotent:\nfirst:  %#v\nsecond: %#v", once.Object, obj.Object)
		}
	})
}

// FuzzColorize pins color as a purely additive, opt-in transform: it is a no-op
// when color is off, and stripping the ANSI escapes from the colored form
// recovers the uncolored form exactly.
func FuzzColorize(f *testing.F) {
	for _, seed := range []string{
		"",
		"plain text\n",
		"+added line\n-removed line\n",
		"@@ -1,3 +1,3 @@\n context\n",
		"--- live\n+++ merged\n",
		"+++not a header\n",
		"no trailing newline",
		"\n\n\n",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, body string) {
		if got := colorize(body, false); got != body {
			t.Fatalf("colorize(_, false) changed input: %q -> %q", body, got)
		}
		colored := colorize(body, true)
		if stripped := stripANSI(colored); stripped != body {
			t.Fatalf("color not additive: strip(colorize(s,true))=%q want %q", stripped, body)
		}
	})
}

// FuzzRenderDiffSummary asserts the Summary returned by RenderDiff counts each
// ChangeKind exactly as it appears in the input, and Changed() iff at least one
// create/configure/delete is present. The change list is built from a fuzzed
// byte slice so the kind sequence is arbitrary.
func FuzzRenderDiffSummary(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4})
	f.Add([]byte{})
	f.Add([]byte{3, 3, 3})
	f.Add(bytes.Repeat([]byte{0, 2}, 8))
	f.Fuzz(func(t *testing.T, kinds []byte) {
		kindByCode := []ChangeKind{ChangeCreate, ChangeConfigure, ChangeDelete, ChangeUnchanged, ChangeSkip}
		var changes []Change
		var want Summary
		for i, b := range kinds {
			k := kindByCode[int(b)%len(kindByCode)]
			ch := Change{Kind: k, GVK: schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, Name: "obj"}
			switch k {
			case ChangeCreate:
				want.Create++
				ch.After = cm("obj", map[string]any{"i": int64(i)})
			case ChangeConfigure:
				want.Configure++
				ch.Before = cm("obj", map[string]any{"i": int64(i)})
				ch.After = cm("obj", map[string]any{"i": int64(i + 1)})
			case ChangeDelete:
				want.Delete++
				ch.Before = cm("obj", map[string]any{"i": int64(i)})
			case ChangeUnchanged:
				want.Unchanged++
			case ChangeSkip:
				want.Skip++
			}
			changes = append(changes, ch)
		}

		var buf bytes.Buffer
		got, err := RenderDiff(&buf, changes, RenderOptions{ShowUnchanged: true})
		if err != nil {
			t.Fatalf("RenderDiff error: %v", err)
		}
		if got != want {
			t.Fatalf("summary counts = %+v, want %+v", got, want)
		}
		wantChanged := want.Create+want.Configure+want.Delete > 0
		if got.Changed() != wantChanged {
			t.Fatalf("Changed()=%v want %v for %+v", got.Changed(), wantChanged, got)
		}
	})
}

// FuzzRenderManifestsSeparator asserts the multi-doc separator count matches the
// object count (n objects → n-1 "---" separators), Secret values never leak,
// and the render never panics on a fuzzed value. The count derives from a
// length byte so empty and single-object inputs are exercised.
func FuzzRenderManifestsSeparator(f *testing.F) {
	f.Add(0, "secret-value")
	f.Add(1, "QQ==")
	f.Add(3, "leaked?")
	f.Add(8, "")
	f.Fuzz(func(t *testing.T, n int, secretVal string) {
		count := ((n%9)+9)%9 + 1 // 1..9, deterministic and bounded
		// A distinctive sentinel prefix makes the leak check robust: the rendered
		// YAML legitimately contains structural keywords ("Secret", "data", …) and
		// the placeholder template, so a bare short value could coincidentally
		// match those. The sentinel cannot appear in structural YAML or the
		// placeholder, so a containment hit means the value itself survived.
		plaintext := "FUZZSENTINEL\x00" + secretVal
		objs := make([]*unstructured.Unstructured, 0, count)
		for i := 0; i < count; i++ {
			objs = append(objs, secretWith(plaintext))
		}

		out, err := RenderManifests(objs, NewSecretMasker(false))
		if err != nil {
			t.Fatalf("RenderManifests error: %v", err)
		}
		if got := strings.Count(out, "---\n"); got != count-1 {
			t.Fatalf("separator count = %d, want %d for %d objects:\n%s", got, count-1, count, out)
		}
		if strings.Contains(out, "FUZZSENTINEL") {
			t.Fatalf("secret plaintext leaked (sentinel survived):\n%s", out)
		}
	})
}

// TestRenderManifests_Empty confirms an empty object list renders to the empty
// string with no separator.
func TestRenderManifests_Empty(t *testing.T) {
	out, err := RenderManifests(nil, NewSecretMasker(false))
	if err != nil {
		t.Fatal(err)
	}
	if out != "" {
		t.Errorf("empty input should render empty, got %q", out)
	}
}

// TestColorize_NoColorIsIdentity is a focused regression alongside the fuzz
// property: color off never inserts an escape byte.
func TestColorize_NoColorIsIdentity(t *testing.T) {
	body := "+add\n-del\n@@ hunk @@\n--- live\n+++ merged\n context\n"
	if got := colorize(body, false); got != body {
		t.Errorf("colorize off changed input: %q", got)
	}
	if strings.Contains(colorize(body, false), "\x1b[") {
		t.Error("color-off output contains an escape byte")
	}
}
