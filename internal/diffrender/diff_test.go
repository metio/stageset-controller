// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package diffrender

import (
	"bytes"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func cm(name string, data map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": name, "namespace": "ns"},
		"data":     data,
	}}
}

func gvk(kind string) schema.GroupVersionKind {
	return schema.GroupVersionKind{Version: "v1", Kind: kind}
}

func render(t *testing.T, changes []Change, opts RenderOptions) (string, Summary) {
	t.Helper()
	var buf bytes.Buffer
	sum, err := RenderDiff(&buf, changes, opts)
	if err != nil {
		t.Fatal(err)
	}
	return buf.String(), sum
}

func TestRenderDiff_Create(t *testing.T) {
	out, sum := render(t, []Change{{
		Stage: "canary", Kind: ChangeCreate, GVK: gvk("ConfigMap"), Namespace: "ns", Name: "settings",
		After: cm("settings", map[string]any{"k": "v"}),
	}}, RenderOptions{})

	if sum.Create != 1 || !sum.Changed() {
		t.Fatalf("summary = %+v", sum)
	}
	if !strings.Contains(out, "create ConfigMap/settings [ns]") {
		t.Errorf("missing create header:\n%s", out)
	}
	if !strings.Contains(out, "stage: canary") {
		t.Errorf("missing stage label:\n%s", out)
	}
}

func TestRenderDiff_Delete(t *testing.T) {
	out, sum := render(t, []Change{{
		Kind: ChangeDelete, GVK: gvk("ConfigMap"), Namespace: "ns", Name: "old",
		Before: cm("old", map[string]any{"k": "v"}),
	}}, RenderOptions{})
	if sum.Delete != 1 {
		t.Fatalf("summary = %+v", sum)
	}
	if !strings.Contains(out, "delete ConfigMap/old") {
		t.Errorf("missing delete header:\n%s", out)
	}
	// A delete renders the live object as fully removed.
	if !strings.Contains(out, "-") {
		t.Errorf("delete body has no removed lines:\n%s", out)
	}
}

func TestRenderDiff_Configure(t *testing.T) {
	out, sum := render(t, []Change{{
		Kind: ChangeConfigure, GVK: gvk("ConfigMap"), Namespace: "ns", Name: "settings",
		Before: cm("settings", map[string]any{"replicas": "2"}),
		After:  cm("settings", map[string]any{"replicas": "5"}),
	}}, RenderOptions{})
	if sum.Configure != 1 {
		t.Fatalf("summary = %+v", sum)
	}
	if !strings.Contains(out, "-  replicas: \"2\"") || !strings.Contains(out, "+  replicas: \"5\"") {
		t.Errorf("unified diff missing changed lines:\n%s", out)
	}
}

func TestRenderDiff_UnchangedHiddenByDefault(t *testing.T) {
	changes := []Change{{Kind: ChangeUnchanged, GVK: gvk("ConfigMap"), Name: "stable"}}

	out, sum := render(t, changes, RenderOptions{})
	if sum.Unchanged != 1 || sum.Changed() {
		t.Fatalf("summary = %+v", sum)
	}
	if strings.Contains(out, "stable") {
		t.Errorf("unchanged object shown without --show-unchanged:\n%s", out)
	}

	out, _ = render(t, changes, RenderOptions{ShowUnchanged: true})
	if !strings.Contains(out, "unchanged ConfigMap/stable") {
		t.Errorf("unchanged object not shown with --show-unchanged:\n%s", out)
	}
}

func TestRenderDiff_NoChanges(t *testing.T) {
	_, sum := render(t, nil, RenderOptions{})
	if sum.Changed() {
		t.Fatal("empty changes should not be Changed()")
	}
}

func TestWriteSummary(t *testing.T) {
	var buf bytes.Buffer
	WriteSummary(&buf, Summary{Create: 2, Delete: 1, Unchanged: 3})
	if got := strings.TrimSpace(buf.String()); got != "Summary: 2 to create, 1 to delete, 3 unchanged" {
		t.Errorf("summary = %q", got)
	}

	buf.Reset()
	WriteSummary(&buf, Summary{})
	if got := strings.TrimSpace(buf.String()); got != "Summary: no changes" {
		t.Errorf("empty summary = %q", got)
	}
}

func TestWriteActions(t *testing.T) {
	var buf bytes.Buffer
	WriteActions(&buf, []ActionPreview{
		{Stage: "canary", Phase: "pre", Name: "maintenance-on", Type: "patch", Detail: "ConfigMap/maint"},
		{Stage: "canary", Phase: "post", Name: "smoke", Type: "http", Detail: "POST https://x"},
	}, false)
	out := buf.String()
	for _, want := range []string{"Actions to run:", "canary:", "maintenance-on", "patch ConfigMap/maint", "smoke", "http POST https://x"} {
		if !strings.Contains(out, want) {
			t.Errorf("actions output missing %q:\n%s", want, out)
		}
	}
}

func TestWriteActions_EmptyIsQuiet(t *testing.T) {
	var buf bytes.Buffer
	WriteActions(&buf, nil, false)
	if buf.Len() != 0 {
		t.Errorf("expected no output for empty actions, got %q", buf.String())
	}
}

func TestWriteMigrations(t *testing.T) {
	var buf bytes.Buffer
	WriteMigrations(&buf, []MigrationPreview{
		{Name: "schema-upgrade", To: "v2", From: "v1", Stage: "canary", Actions: 2},
		{Name: "seed", To: "v2", Stage: "prod", Actions: 1},
	}, false)
	out := buf.String()
	for _, want := range []string{"Migrations to run:", "schema-upgrade", "v1 → v2", "before stage canary", "2 actions", "→ v2", "1 action"} {
		if !strings.Contains(out, want) {
			t.Errorf("migrations output missing %q:\n%s", want, out)
		}
	}
}

func TestWriteMigrations_EmptyIsQuiet(t *testing.T) {
	var buf bytes.Buffer
	WriteMigrations(&buf, nil, false)
	if buf.Len() != 0 {
		t.Errorf("expected no output for no migrations, got %q", buf.String())
	}
}

func TestRenderDiff_MasksSecret(t *testing.T) {
	before := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": "creds", "namespace": "ns"},
		"data": map[string]any{"password": "b2xk"},
	}}
	after := before.DeepCopy()
	_ = unstructured.SetNestedField(after.Object, "bmV3", "data", "password")

	out, _ := render(t, []Change{{
		Kind: ChangeConfigure, GVK: gvk("Secret"), Namespace: "ns", Name: "creds",
		Before: before, After: after,
	}}, RenderOptions{})

	if strings.Contains(out, "b2xk") || strings.Contains(out, "bmV3") {
		t.Errorf("secret plaintext leaked:\n%s", out)
	}
}

func TestRenderDiff_Color(t *testing.T) {
	out, _ := render(t, []Change{{
		Kind: ChangeCreate, GVK: gvk("ConfigMap"), Name: "x", After: cm("x", map[string]any{"k": "v"}),
	}}, RenderOptions{Color: true})
	if !strings.Contains(out, ansiGreen) || !strings.Contains(out, ansiReset) {
		t.Errorf("expected ANSI color codes:\n%q", out)
	}
}

func TestRenderDiff_NoColorByDefault(t *testing.T) {
	out, _ := render(t, []Change{{
		Kind: ChangeCreate, GVK: gvk("ConfigMap"), Name: "x", After: cm("x", map[string]any{"k": "v"}),
	}}, RenderOptions{Color: false})
	if strings.Contains(out, "\x1b[") {
		t.Errorf("unexpected ANSI codes with color off:\n%q", out)
	}
}

func TestSummary_Changed(t *testing.T) {
	if (Summary{Unchanged: 3}).Changed() {
		t.Error("only-unchanged should not be Changed()")
	}
	if !(Summary{Delete: 1}).Changed() {
		t.Error("a delete should be Changed()")
	}
}
