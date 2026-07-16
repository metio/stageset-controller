// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

// Package build renders a stage's source artifact into apply-ready Kubernetes
// objects: a secure kustomize build (generating a kustomization.yaml when the
// artifact has none, and layering the stage's patches) followed by post-build
// variable substitution. It is deliberately free of any Kubernetes client —
// substituteFrom values are resolved by the caller and passed in as `vars` —
// so the whole render path is unit-testable without a cluster.
package build

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	apiskustomize "github.com/fluxcd/pkg/apis/kustomize"
	"github.com/fluxcd/pkg/envsubst"
	"github.com/fluxcd/pkg/kustomize"
	"github.com/fluxcd/pkg/ssa/normalize"
	ssautils "github.com/fluxcd/pkg/ssa/utils"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// placeholderNamespace is the name the Flux generator injects for an otherwise
// empty build directory (kustomize rejects an empty kustomization). It is not
// a real resource and is dropped from the output.
const placeholderNamespace = "_placeholder"

// manifestExts are the extensions a build can produce objects from. kustomize's
// scanner reads .yaml/.yml, and normalizeJSONManifests brings .json in; a
// kustomization.yaml is itself a .yaml, so a directory holding none of these
// cannot yield an object by any route — including a remote-resource
// kustomization, which needs a kustomization.yaml to declare one.
var manifestExts = map[string]bool{".yaml": true, ".yml": true, ".json": true}

// ErrNoManifests reports a build whose path holds no file a manifest could come
// from. It is deliberately about FILES, not objects: a source that publishes
// nothing is a mistake, while a render that legitimately produces zero objects
// (a JsonnetSnippet emitting [] for a disabled feature) is a real pattern and
// stays permitted — that artifact still carries its rendered.json.
//
// Refusing an empty artifact does withdraw one behavior: it used to mean "prune
// everything this stage owns". That reading is not worth keeping. Removing the
// stage from spec.stages already tears its objects down in reverse recorded
// order, and does so explicitly — a reviewable spec change, through admission.
// An emptied artifact says the same thing implicitly, from another repository,
// while the spec still asks for a deployment. The two also fail in opposite
// directions: a bad ignore rule cannot delete a stage from a spec, but it can
// empty an artifact, so the permissive reading turns a typo into a mass
// deletion of everything the stage owns.
var ErrNoManifests = errors.New("artifact contains no .yaml, .yml, or .json files")

// hasManifests reports whether any file under path could yield a manifest.
// path is the artifact-relative build directory; empty means the root.
func hasManifests(files map[string]string, path string) bool {
	prefix := strings.Trim(path, "/")
	if prefix == "" || prefix == "." {
		prefix = ""
	} else {
		prefix = filepath.Clean(prefix) + string(os.PathSeparator)
	}
	for name := range files {
		clean := filepath.Clean(name)
		if prefix != "" && !strings.HasPrefix(clean, prefix) {
			continue
		}
		if manifestExts[strings.ToLower(filepath.Ext(clean))] {
			return true
		}
	}
	return false
}

// Options carries the per-stage build inputs.
type Options struct {
	// Path is the directory within the artifact to build (stage.Path); empty
	// means the artifact root.
	Path string
	// Patches are the stage's Kustomize patches, layered onto the build.
	Patches []apiskustomize.Patch
}

// Build materializes files onto disk, runs a secure kustomize build at
// opts.Path (generating a kustomization.yaml if absent and applying patches),
// substitutes ${var} references using vars, and returns the rendered objects.
// vars may be nil/empty to skip substitution.
func Build(files map[string]string, opts Options, vars map[string]string) ([]*unstructured.Unstructured, error) {
	root, cleanup, err := materialize(files)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	buildDir := root
	if p := strings.Trim(opts.Path, "/"); p != "" && p != "." {
		buildDir = filepath.Join(root, filepath.Clean(p))
		if !strings.HasPrefix(buildDir, root+string(os.PathSeparator)) {
			return nil, fmt.Errorf("build path %q escapes the artifact root", opts.Path)
		}
		if fi, statErr := os.Stat(buildDir); statErr != nil || !fi.IsDir() {
			return nil, fmt.Errorf("build path %q not found in artifact", opts.Path)
		}
	}

	// Fail here rather than let the build succeed with nothing. kustomize's
	// generator injects a placeholder namespace for an empty directory and the
	// output drops it, so the stage would apply zero objects, record itself
	// applied, and only fail minutes later at readyChecks — naming objects that
	// were never in the artifact instead of the source that never shipped them.
	// The ladder path (migrations.ParseLadder) already refuses this; a stage
	// source in the same state should read the same way.
	//
	// Checked after the path resolution above so a mistyped path keeps its more
	// precise error.
	if !hasManifests(files, opts.Path) {
		if p := strings.Trim(opts.Path, "/"); p != "" && p != "." {
			return nil, fmt.Errorf("%w under path %q (wrong path, or the source published something else)", ErrNoManifests, opts.Path)
		}
		return nil, fmt.Errorf("%w (the source published something else, or its ignore rules pruned everything)", ErrNoManifests)
	}

	// The kustomize resource scanner only picks up .yaml/.yml files, but a
	// producer may publish a rendered manifest as .json — a JaaS JsonnetSnippet
	// publishes its whole rendered output as a single rendered.json. Without this
	// the scanner skips it and the build applies nothing. JSON is a subset of
	// YAML, so renaming .json manifests to .yaml lets kustomize parse the same
	// bytes. Skipped when the build root carries its own kustomization (it selects
	// resources explicitly, and an explicitly-listed .json is already accepted).
	if !hasKustomization(buildDir) {
		if err := normalizeJSONManifests(root, buildDir, files); err != nil {
			return nil, fmt.Errorf("normalize json manifests: %w", err)
		}
	}

	fluxK, err := fluxKustomization(opts)
	if err != nil {
		return nil, err
	}

	gen := kustomize.NewGenerator(root, fluxK)
	action, err := gen.WriteFile(buildDir)
	if err != nil {
		return nil, fmt.Errorf("generate kustomization: %w", err)
	}
	defer func() { _ = kustomize.CleanDirectory(buildDir, action) }()

	resMap, err := kustomize.SecureBuild(root, buildDir, false)
	if err != nil {
		return nil, fmt.Errorf("kustomize build: %w", err)
	}
	yamlBytes, err := resMap.AsYaml()
	if err != nil {
		return nil, fmt.Errorf("render build output: %w", err)
	}

	if len(vars) > 0 {
		substituted, serr := envsubst.Eval(string(yamlBytes), func(k string) (string, bool) {
			v, ok := vars[k]
			return v, ok
		})
		if serr != nil {
			return nil, fmt.Errorf("post-build substitution: %w", serr)
		}
		yamlBytes = []byte(substituted)
	}

	objects, err := ssautils.ReadObjects(bytes.NewReader(yamlBytes))
	if err != nil {
		return nil, fmt.Errorf("parse build output: %w", err)
	}
	if err := normalize.UnstructuredList(objects); err != nil {
		return nil, fmt.Errorf("normalize objects: %w", err)
	}
	return dropPlaceholder(objects), nil
}

// fluxKustomization synthesizes the Flux-Kustomization-shaped unstructured the
// generator reads its fields from. Only spec.patches is populated here; other
// fields (targetNamespace, images, …) are future stage options.
func fluxKustomization(opts Options) (unstructured.Unstructured, error) {
	spec := map[string]any{}
	if len(opts.Patches) > 0 {
		raw, err := json.Marshal(opts.Patches)
		if err != nil {
			return unstructured.Unstructured{}, fmt.Errorf("encode patches: %w", err)
		}
		var patches []any
		if err := json.Unmarshal(raw, &patches); err != nil {
			return unstructured.Unstructured{}, fmt.Errorf("decode patches: %w", err)
		}
		spec["patches"] = patches
	}
	return unstructured.Unstructured{Object: map[string]any{"spec": spec}}, nil
}

func dropPlaceholder(objects []*unstructured.Unstructured) []*unstructured.Unstructured {
	out := objects[:0]
	for _, o := range objects {
		if o.GetKind() == "Namespace" && o.GetName() == placeholderNamespace {
			continue
		}
		out = append(out, o)
	}
	return out
}

// materialize writes the path->content map into a fresh temp directory,
// creating intermediate directories. Entry paths are re-validated (no
// absolute paths, no "..") even though the fetcher already filters them.
func materialize(files map[string]string) (root string, cleanup func(), err error) {
	root, err = os.MkdirTemp("", "stageset-build-*")
	if err != nil {
		return "", nil, fmt.Errorf("create build dir: %w", err)
	}
	cleanup = func() { _ = os.RemoveAll(root) }

	for name, content := range files {
		clean := filepath.Clean(name)
		if filepath.IsAbs(clean) || strings.HasPrefix(clean, "..") {
			cleanup()
			return "", nil, fmt.Errorf("unsafe artifact entry %q", name)
		}
		dest := filepath.Join(root, clean)
		if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
			cleanup()
			return "", nil, fmt.Errorf("create dir for %q: %w", name, err)
		}
		if err := os.WriteFile(dest, []byte(content), 0o600); err != nil {
			cleanup()
			return "", nil, fmt.Errorf("write %q: %w", name, err)
		}
	}
	return root, cleanup, nil
}

// hasKustomization reports whether dir holds a kustomization file kustomize would
// treat as the build root (rather than auto-generating one by scanning).
func hasKustomization(dir string) bool {
	for _, n := range []string{"kustomization.yaml", "kustomization.yml", "Kustomization"} {
		if _, err := os.Stat(filepath.Join(dir, n)); err == nil {
			return true
		}
	}
	return false
}

// normalizeJSONManifests renames the artifact's *.json entries that sit directly
// in the build root to *.yaml so kustomize's .yaml/.yml-only resource scanner
// includes them (JSON is a subset of YAML, so the bytes parse unchanged). This
// covers a producer that publishes its rendered output as a single file at the
// root — a JaaS JsonnetSnippet's rendered.json. It works off the trusted `files`
// map (materialize already rejected absolute and traversing entries) rather than
// walking the filesystem, so a swapped symlink cannot redirect the rename. A
// .json that already has a .yaml sibling is left alone.
func normalizeJSONManifests(root, buildDir string, files map[string]string) error {
	buildDir = filepath.Clean(buildDir)
	for name := range files {
		dest := filepath.Join(root, filepath.Clean(name))
		if filepath.Dir(dest) != buildDir || filepath.Ext(dest) != ".json" {
			continue
		}
		yamlPath := strings.TrimSuffix(dest, ".json") + ".yaml"
		if _, err := os.Stat(yamlPath); err == nil {
			continue
		}
		if err := os.Rename(dest, yamlPath); err != nil {
			return err
		}
	}
	return nil
}
