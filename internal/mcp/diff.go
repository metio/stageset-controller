/*
 * SPDX-FileCopyrightText: The stageset-controller Authors
 * SPDX-License-Identifier: 0BSD
 */

package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/diffrender"
	"github.com/metio/stageset-controller/internal/rollbackstore"
)

// RollbackReader is the read side of the rollback store the diff_revisions tool
// needs. Both rollbackstore backends (FileStore, S3Store) satisfy it. Kept local
// so this package takes no dependency on the controller package — only on the
// store's addressing contract (rollbackstore.Key).
type RollbackReader interface {
	Get(ctx context.Context, key string) (data []byte, found bool, err error)
}

// errSnapshotMissing marks a revision absent from the rollback store (store
// disabled, or that digest was never a successful apply / has rotated out).
var errSnapshotMissing = errors.New("revision not in rollback store")

// maxDiffSnapshotBytes bounds a single stored snapshot the diff tool decodes
// into memory. The rollback store holds the controller's own rendered output, so
// this is a heap guard, not an untrusted-input defense.
const maxDiffSnapshotBytes = 32 << 20 // 32 MiB

type diffRevisionsInput struct {
	Namespace string `json:"namespace" jsonschema:"the StageSet's namespace"`
	Name      string `json:"name" jsonschema:"the StageSet's name"`
	Stage     string `json:"stage" jsonschema:"the stage whose rendered output to diff (the rollback store is per-stage)"`
	From      string `json:"from" jsonschema:"the earlier artifact digest (algo:hex) to diff from; must be retained in the rollback store"`
	To        string `json:"to,omitempty" jsonschema:"the later artifact digest; defaults to the stage's currently-applied digest from status.lastAppliedSnapshot"`
}

type diffRevisionsOutput struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Stage     string `json:"stage"`
	From      string `json:"from"`
	To        string `json:"to"`
	Diff      string `json:"diff" jsonschema:"per-object unified diff of the stage's rendered manifests between the two revisions; empty when identical. Secret values are masked."`
	Create    int    `json:"create" jsonschema:"objects only in the to revision"`
	Configure int    `json:"configure" jsonschema:"objects present in both but changed"`
	Delete    int    `json:"delete" jsonschema:"objects only in the from revision"`
	Unchanged int    `json:"unchanged"`
}

func (cfg Config) diffRevisionsHandler(ctx context.Context, _ *mcpsdk.CallToolRequest, in diffRevisionsInput) (*mcpsdk.CallToolResult, diffRevisionsOutput, error) {
	if in.Namespace == "" || in.Name == "" || in.Stage == "" {
		return errorResult("namespace, name, and stage are required"), diffRevisionsOutput{}, nil
	}
	if in.From == "" {
		return errorResult("from (an earlier artifact digest) is required"), diffRevisionsOutput{}, nil
	}
	// The digest becomes part of the rollback-store key (and, on the S3 backend,
	// the object name), so a caller-supplied digest must be a plain algo:hex
	// value — never a path-bearing string that could escape the key prefix.
	if err := rollbackstore.ValidDigest(in.From); err != nil {
		return errorResult(fmt.Sprintf("invalid from digest: %v", err)), diffRevisionsOutput{}, nil
	}

	var ss stagesv1.StageSet
	if err := cfg.KubeClient.Get(ctx, client.ObjectKey{Namespace: in.Namespace, Name: in.Name}, &ss); err != nil {
		return errorResult(fmt.Sprintf("cannot get StageSet %s/%s: %v", in.Namespace, in.Name, err)), diffRevisionsOutput{}, nil
	}
	// Bind the requested stage to a real declared stage: it scopes the rollback
	// store lookup, and an arbitrary caller-supplied stage would otherwise read a
	// snapshot for a stage the StageSet never declared. It also keeps the stage a
	// DNS-1123 name, so the rollback-store key (flattened to a filename) can't be
	// made non-injective with a "_"-bearing component.
	if !stageDeclared(&ss, in.Stage) {
		return errorResult(fmt.Sprintf("stage %q is not declared on StageSet %s/%s", in.Stage, in.Namespace, in.Name)), diffRevisionsOutput{}, nil
	}

	to := in.To
	if to == "" {
		to = appliedDigest(&ss, in.Stage)
		if to == "" {
			return errorResult(fmt.Sprintf("stage %q has no applied snapshot to default 'to' from; pass an explicit to digest", in.Stage)), diffRevisionsOutput{}, nil
		}
	} else if err := rollbackstore.ValidDigest(to); err != nil {
		// Only a caller-supplied 'to' needs checking; the defaulted value comes
		// from the StageSet's own verified status.
		return errorResult(fmt.Sprintf("invalid to digest: %v", err)), diffRevisionsOutput{}, nil
	}
	// Diffing a revision against itself reads the same snapshot twice and reports
	// an all-unchanged result an agent can't distinguish from two distinct
	// revisions that rendered identically. Fail fast — also covers 'to'
	// defaulting to the applied digest when the caller passes that same digest as
	// 'from'.
	if in.From == to {
		return errorResult(fmt.Sprintf("from and to are the same revision %s; nothing to diff", to)), diffRevisionsOutput{}, nil
	}

	fromObjs, err := cfg.readSnapshot(ctx, in.Namespace, in.Name, in.Stage, in.From)
	if err != nil {
		return errorResult(snapshotReadError("from", in.From, err)), diffRevisionsOutput{}, nil
	}
	toObjs, err := cfg.readSnapshot(ctx, in.Namespace, in.Name, in.Stage, to)
	if err != nil {
		return errorResult(snapshotReadError("to", to, err)), diffRevisionsOutput{}, nil
	}

	changes := changesBetween(in.Stage, fromObjs, toObjs)
	var buf bytes.Buffer
	sum, err := diffrender.RenderDiff(&buf, changes, diffrender.RenderOptions{Masker: diffrender.NewSecretMasker(false)})
	if err != nil {
		return errorResult(fmt.Sprintf("rendering the diff failed: %v", err)), diffRevisionsOutput{}, nil
	}

	return nil, diffRevisionsOutput{
		Namespace: in.Namespace,
		Name:      in.Name,
		Stage:     in.Stage,
		From:      in.From,
		To:        to,
		Diff:      buf.String(),
		Create:    sum.Create,
		Configure: sum.Configure,
		Delete:    sum.Delete,
		Unchanged: sum.Unchanged,
	}, nil
}

// stageDeclared reports whether stage is one of the StageSet's declared stages.
func stageDeclared(ss *stagesv1.StageSet, stage string) bool {
	for i := range ss.Spec.Stages {
		if ss.Spec.Stages[i].Name == stage {
			return true
		}
	}
	return false
}

// appliedDigest returns the artifact digest the named stage currently has
// applied, from status.lastAppliedSnapshot, or "" when the stage isn't recorded.
func appliedDigest(ss *stagesv1.StageSet, stage string) string {
	for _, r := range ss.Status.LastAppliedSnapshot {
		if r.Stage == stage {
			return r.Digest
		}
	}
	return ""
}

func (cfg Config) readSnapshot(ctx context.Context, namespace, name, stage, digest string) ([]*unstructured.Unstructured, error) {
	data, found, err := cfg.RollbackStore.Get(ctx, rollbackstore.Key(namespace, name, stage, digest))
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errSnapshotMissing
	}
	if len(data) > maxDiffSnapshotBytes {
		return nil, fmt.Errorf("snapshot exceeds %d bytes; too large to diff", int64(maxDiffSnapshotBytes))
	}
	var raw []map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("decode snapshot: %w", err)
	}
	out := make([]*unstructured.Unstructured, len(raw))
	for i, m := range raw {
		out[i] = &unstructured.Unstructured{Object: m}
	}
	return out, nil
}

// changesBetween classifies every object across the two snapshots into a
// diffrender.Change. The snapshots are rendered manifests (not live objects), so
// a plain deep-equal is an accurate unchanged test — there is no server noise to
// strip first. Output order is stable (sorted by GVK, namespace, name).
func changesBetween(stage string, from, to []*unstructured.Unstructured) []diffrender.Change {
	type objKey struct {
		gvk       schema.GroupVersionKind
		namespace string
		name      string
	}
	keyOf := func(o *unstructured.Unstructured) objKey {
		return objKey{o.GroupVersionKind(), o.GetNamespace(), o.GetName()}
	}
	fromBy := make(map[objKey]*unstructured.Unstructured, len(from))
	for _, o := range from {
		fromBy[keyOf(o)] = o
	}
	toBy := make(map[objKey]*unstructured.Unstructured, len(to))
	for _, o := range to {
		toBy[keyOf(o)] = o
	}

	seen := map[objKey]struct{}{}
	keys := make([]objKey, 0, len(fromBy)+len(toBy))
	for k := range fromBy {
		if _, ok := seen[k]; !ok {
			seen[k] = struct{}{}
			keys = append(keys, k)
		}
	}
	for k := range toBy {
		if _, ok := seen[k]; !ok {
			seen[k] = struct{}{}
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.gvk.String() != b.gvk.String() {
			return a.gvk.String() < b.gvk.String()
		}
		if a.namespace != b.namespace {
			return a.namespace < b.namespace
		}
		return a.name < b.name
	})

	changes := make([]diffrender.Change, 0, len(keys))
	for _, k := range keys {
		before, after := fromBy[k], toBy[k]
		ch := diffrender.Change{Stage: stage, GVK: k.gvk, Namespace: k.namespace, Name: k.name, Before: before, After: after}
		switch {
		case before == nil:
			ch.Kind = diffrender.ChangeCreate
		case after == nil:
			ch.Kind = diffrender.ChangeDelete
		case reflect.DeepEqual(before.Object, after.Object):
			ch.Kind = diffrender.ChangeUnchanged
		default:
			ch.Kind = diffrender.ChangeConfigure
		}
		changes = append(changes, ch)
	}
	return changes
}

func snapshotReadError(which, digest string, err error) string {
	if errors.Is(err, errSnapshotMissing) {
		return fmt.Sprintf("%s revision %s is not in the rollback store (the store is disabled, or that revision was never applied / has rotated out)", which, digest)
	}
	return fmt.Sprintf("cannot read %s revision %s: %v", which, digest, err)
}
