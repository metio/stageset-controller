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
