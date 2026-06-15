// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

// Package preview renders a StageSet's stages into apply-ready objects using
// the controller's own resolve→fetch→build path, so a CLI preview matches what
// the controller applies. Sources come from the cluster (resolve the
// ExternalArtifact, fetch the tarball) or from a local directory supplied by
// the operator when the artifact storage is unreachable.
package preview

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/artifact"
	"github.com/metio/stageset-controller/internal/build"
)

// Engine renders stages for a StageSet. Construct one per command invocation.
type Engine struct {
	// Client reads the ExternalArtifact, ConfigMaps, and Secrets a render
	// needs. Pass an impersonating client to render as the tenant SA.
	Client client.Client

	// Resolver and Fetcher mirror the controller's source path.
	Resolver *artifact.Resolver
	Fetcher  *artifact.Fetcher

	// SourceDirs maps a stage name to a local artifact root, bypassing the
	// cluster fetch. The empty-string key, if present, applies to every stage
	// without its own entry. Used when the artifact storage URL is unreachable
	// or for fully offline rendering.
	SourceDirs map[string]string
}

// NewEngine builds an Engine with a permissive fetcher: the operator is dialing
// their own cluster's storage, so the controller's SSRF denylist (aimed at
// untrusted snippet-supplied URLs) does not apply.
func NewEngine(c client.Client, noCrossNamespace bool) *Engine {
	f := artifact.New()
	f.URLValidator = func(string) error { return nil }
	f.IPValidator = func(net.IP) error { return nil }
	return &Engine{
		Client:   c,
		Resolver: &artifact.Resolver{NoCrossNamespace: noCrossNamespace},
		Fetcher:  f,
	}
}

// StageRender is the rendered output of one stage.
type StageRender struct {
	Stage    string
	Objects  []*unstructured.Unstructured
	Revision string // artifact revision, empty when rendered from a local dir
	Local    bool   // true when the source came from SourceDirs
}

// SelectStages returns the stages to render: all of them when names is empty,
// otherwise the named subset in spec order. An unknown name is an error so a
// typo never silently renders nothing.
func SelectStages(ss *stagesv1.StageSet, names []string) ([]stagesv1.Stage, error) {
	if len(names) == 0 {
		return ss.Spec.Stages, nil
	}
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	var out []stagesv1.Stage
	for _, st := range ss.Spec.Stages {
		if want[st.Name] {
			out = append(out, st)
			delete(want, st.Name)
		}
	}
	if len(want) > 0 {
		for _, n := range names {
			if want[n] {
				return nil, fmt.Errorf("stage %q not found in StageSet %q", n, ss.Name)
			}
		}
	}
	return out, nil
}

// RenderStage resolves the stage's source (cluster or local dir), runs the
// kustomize build with the stage's patches and post-build substitutions, and
// returns the apply-ready objects.
func (e *Engine) RenderStage(ctx context.Context, ss *stagesv1.StageSet, stage *stagesv1.Stage) (StageRender, error) {
	files, revision, local, err := e.sourceFiles(ctx, ss, stage)
	if err != nil {
		return StageRender{}, err
	}
	vars, err := resolvePostBuildVars(ctx, e.Client, ss.Namespace, stage.PostBuild)
	if err != nil {
		return StageRender{}, err
	}
	objs, err := build.Build(files, build.Options{Path: stage.Path, Patches: stage.Patches}, vars)
	if err != nil {
		return StageRender{}, fmt.Errorf("build stage %q: %w", stage.Name, err)
	}
	return StageRender{Stage: stage.Name, Objects: objs, Revision: revision, Local: local}, nil
}

// sourceFiles returns the artifact file tree for a stage, preferring a local
// SourceDir over a cluster fetch.
func (e *Engine) sourceFiles(ctx context.Context, ss *stagesv1.StageSet, stage *stagesv1.Stage) (files map[string]string, revision string, local bool, err error) {
	if dir, ok := e.sourceDir(stage.Name); ok {
		files, err = readDirFiles(dir)
		if err != nil {
			return nil, "", false, fmt.Errorf("read --source-dir for stage %q: %w", stage.Name, err)
		}
		return files, "", true, nil
	}
	ra, err := e.Resolver.Resolve(ctx, e.Client, stage.SourceRef, ss.Namespace)
	if err != nil {
		return nil, "", false, fmt.Errorf("resolve source for stage %q: %w", stage.Name, err)
	}
	files, err = e.Fetcher.Fetch(ctx, ra.URL, ra.Digest, "")
	if err != nil {
		return nil, "", false, fmt.Errorf("fetch artifact for stage %q: %w", stage.Name, err)
	}
	return files, ra.Revision, false, nil
}

func (e *Engine) sourceDir(stage string) (string, bool) {
	if e.SourceDirs == nil {
		return "", false
	}
	if dir, ok := e.SourceDirs[stage]; ok {
		return dir, true
	}
	if dir, ok := e.SourceDirs[""]; ok {
		return dir, true
	}
	return "", false
}

// readDirFiles walks dir and returns a path→content map keyed by slash-relative
// paths, matching what the fetcher produces from a tarball.
func readDirFiles(dir string) (map[string]string, error) {
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		// #nosec G304 G122 -- dir is an operator-supplied path the same user
		// already has shell access to; this is a local render of their own
		// files, not a server reading untrusted input, so symlink traversal
		// within their tree is their choice, not a privilege boundary.
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = string(content)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no files under %q", dir)
	}
	return files, nil
}

// resolvePostBuildVars mirrors the controller's substitution-variable
// resolution: substituteFrom ConfigMaps/Secrets folded in first, then inline
// substitute values winning on conflict.
func resolvePostBuildVars(ctx context.Context, c client.Client, ns string, pb *stagesv1.PostBuild) (map[string]string, error) {
	if pb == nil {
		return nil, nil
	}
	vars := map[string]string{}
	for _, ref := range pb.SubstituteFrom {
		key := types.NamespacedName{Namespace: ns, Name: ref.Name}
		switch ref.Kind {
		case "ConfigMap":
			var cm corev1.ConfigMap
			if err := c.Get(ctx, key, &cm); err != nil {
				if ref.Optional && apierrors.IsNotFound(err) {
					continue
				}
				return nil, fmt.Errorf("substituteFrom ConfigMap %q: %w", ref.Name, err)
			}
			for k, v := range cm.Data {
				vars[k] = v
			}
		case "Secret":
			var sec corev1.Secret
			if err := c.Get(ctx, key, &sec); err != nil {
				if ref.Optional && apierrors.IsNotFound(err) {
					continue
				}
				return nil, fmt.Errorf("substituteFrom Secret %q: %w", ref.Name, err)
			}
			for k, v := range sec.Data {
				vars[k] = string(v)
			}
		}
	}
	for k, v := range pb.Substitute {
		vars[k] = v
	}
	return vars, nil
}
