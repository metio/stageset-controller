// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package preview

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

// FuzzSelectStages drives SelectStages with arbitrary spec/request name lists
// and asserts its invariants hold for every input:
//   - the result is a subset of the spec stages,
//   - it preserves spec order,
//   - requesting a name absent from the spec is an error,
//   - an empty request returns every spec stage.
func FuzzSelectStages(f *testing.F) {
	f.Add("a,b,c", "a,c")
	f.Add("a,b,c", "")
	f.Add("a,b,c", "ghost")
	f.Add("", "x")
	f.Add("a", "a,a")

	f.Fuzz(func(t *testing.T, specCSV, reqCSV string) {
		specNames := splitNonEmpty(specCSV)
		ss := stageSet(specNames...)

		specSet := map[string]bool{}
		for _, n := range specNames {
			specSet[n] = true
		}

		var req []string
		anyUnknown := false
		for _, n := range splitNonEmpty(reqCSV) {
			req = append(req, n)
			if !specSet[n] {
				anyUnknown = true
			}
		}

		got, err := SelectStages(ss, req)

		if anyUnknown {
			if err == nil {
				t.Fatalf("requesting an unknown stage must error: spec=%q req=%q", specCSV, reqCSV)
			}
			return
		}
		if err != nil {
			t.Fatalf("unexpected error for spec=%q req=%q: %v", specCSV, reqCSV, err)
		}

		if len(req) == 0 {
			if len(got) != len(ss.Spec.Stages) {
				t.Fatalf("empty request must return all %d stages, got %d", len(ss.Spec.Stages), len(got))
			}
			return
		}

		// Every returned stage must exist in the spec, and the result must
		// follow spec order (each result is at a strictly increasing spec
		// index). Duplicates in the request collapse to a single occurrence.
		lastIdx := -1
		for _, st := range got {
			if !specSet[st.Name] {
				t.Fatalf("result stage %q not in spec", st.Name)
			}
			idx := specIndex(specNames, st.Name)
			if idx <= lastIdx {
				t.Fatalf("result not in spec order: %q at index %d after %d", st.Name, idx, lastIdx)
			}
			lastIdx = idx
		}
	})
}

// FuzzReadDirFiles writes a single file under a temp dir at an arbitrary
// (sanitized) relative path and asserts readDirFiles returns it keyed by a
// slash-relative path that never escapes the root.
func FuzzReadDirFiles(f *testing.F) {
	f.Add("a.yaml", "x")
	f.Add("sub/b.yaml", "y")
	f.Add("deep/nested/c.txt", "")

	f.Fuzz(func(t *testing.T, rel, content string) {
		clean := sanitizeRel(rel)
		if clean == "" {
			return
		}
		dir := t.TempDir()
		target := filepath.Join(dir, filepath.FromSlash(clean))
		if !strings.HasPrefix(target, dir+string(os.PathSeparator)) {
			return // sanitization should prevent this, but never write outside the root
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}

		files, err := readDirFiles(dir)
		if err != nil {
			t.Fatalf("readDirFiles(%q): %v", clean, err)
		}
		got, ok := files[clean]
		if !ok {
			t.Fatalf("missing key %q in %v", clean, files)
		}
		if got != content {
			t.Fatalf("content mismatch for %q: want %q got %q", clean, content, got)
		}
		for k := range files {
			if strings.HasPrefix(k, "/") || strings.Contains(k, "..") {
				t.Fatalf("key %q escapes the root", k)
			}
		}
	})
}

func splitNonEmpty(csv string) []string {
	var out []string
	for _, p := range strings.Split(csv, ",") {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func specIndex(names []string, name string) int {
	for i, n := range names {
		if n == name {
			return i
		}
	}
	return -1
}

// sanitizeRel reduces a fuzz-supplied path to a safe slash-relative path under
// a temp dir, dropping any segment that would escape the root or break the
// filesystem write.
func sanitizeRel(rel string) string {
	rel = strings.ReplaceAll(rel, "\\", "/")
	var segs []string
	for _, seg := range strings.Split(rel, "/") {
		if seg == "" || seg == "." || seg == ".." {
			continue
		}
		if strings.ContainsRune(seg, 0) {
			continue
		}
		segs = append(segs, seg)
	}
	return strings.Join(segs, "/")
}

// TestSourceDir_StageKeyWinsOverDefault confirms a stage-specific SourceDirs
// entry takes precedence over the empty-key default.
func TestSourceDir_StageKeyWinsOverDefault(t *testing.T) {
	e := &Engine{SourceDirs: map[string]string{"": "/default", "special": "/special"}}

	if dir, ok := e.sourceDir("special"); !ok || dir != "/special" {
		t.Fatalf("stage key must win: got %q ok=%v", dir, ok)
	}
	if dir, ok := e.sourceDir("other"); !ok || dir != "/default" {
		t.Fatalf("empty-key default must apply: got %q ok=%v", dir, ok)
	}

	none := &Engine{}
	if _, ok := none.sourceDir("x"); ok {
		t.Fatal("nil SourceDirs must report no dir")
	}

	noDefault := &Engine{SourceDirs: map[string]string{"only": "/o"}}
	if _, ok := noDefault.sourceDir("missing"); ok {
		t.Fatal("absent stage with no default must report no dir")
	}
}

// TestRenderStage_SourceDirReadError surfaces the wrapped error when the
// configured --source-dir does not exist.
func TestRenderStage_SourceDirReadError(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	engine := NewEngine(c, false)
	engine.SourceDirs = map[string]string{"": filepath.Join(t.TempDir(), "does-not-exist")}

	ss := stageSet("s1")
	_, err := engine.RenderStage(context.Background(), ss, &ss.Spec.Stages[0])
	if err == nil || !strings.Contains(err.Error(), "read --source-dir") {
		t.Fatalf("want read --source-dir error, got %v", err)
	}
}

// TestRenderStage_BuildError feeds a source dir whose only file is invalid
// kustomize input, exercising the build error path.
func TestRenderStage_BuildError(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "broken.yaml"), ": not: valid: yaml: at all")

	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	engine := NewEngine(c, false)
	engine.SourceDirs = map[string]string{"": dir}

	ss := stageSet("s1")
	_, err := engine.RenderStage(context.Background(), ss, &ss.Spec.Stages[0])
	if err == nil || !strings.Contains(err.Error(), "build stage") {
		t.Fatalf("want build stage error, got %v", err)
	}
}

// TestRenderStage_ResolveError exercises the cluster-fetch branch when no
// SourceDir is set and the ExternalArtifact cannot be resolved.
func TestRenderStage_ResolveError(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	engine := NewEngine(c, false)

	ss := stageSet("s1")
	ss.Spec.Stages[0].SourceRef = stagesv1.SourceReference{Name: "missing-artifact"}

	_, err := engine.RenderStage(context.Background(), ss, &ss.Spec.Stages[0])
	if err == nil || !strings.Contains(err.Error(), "resolve source") {
		t.Fatalf("want resolve source error, got %v", err)
	}
}

// TestRenderStage_PostBuildError fails before the build when a required
// substituteFrom ConfigMap is missing.
func TestRenderStage_PostBuildError(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "cm.yaml"), "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n")

	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	engine := NewEngine(c, false)
	engine.SourceDirs = map[string]string{"": dir}

	ss := stageSet("s1")
	ss.Spec.Stages[0].PostBuild = &stagesv1.PostBuild{
		SubstituteFrom: []stagesv1.SubstituteReference{{Kind: "ConfigMap", Name: "gone"}},
	}

	_, err := engine.RenderStage(context.Background(), ss, &ss.Spec.Stages[0])
	if err == nil || !strings.Contains(err.Error(), "substituteFrom") {
		t.Fatalf("want substituteFrom error, got %v", err)
	}
}
