/*
 * SPDX-FileCopyrightText: The stageset-controller Authors
 * SPDX-License-Identifier: 0BSD
 */

package mcp

import (
	"context"
	"errors"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/rollbackstore"
)

// errRollback is a RollbackReader whose Get always fails, so a test can drive the
// store-error path that the in-memory fakeRollback never produces.
type errRollback struct{ err error }

func (e errRollback) Get(_ context.Context, _ string) ([]byte, bool, error) {
	return nil, false, e.err
}

// diffStageSet builds a StageSet with one declared stage and that stage's applied
// digest recorded, so an omitted 'to' defaults to it. Mirrors the local helper in
// diff_test.go without duplicating it.
func diffStageSet(ns, name, stage, applied string) *stagesv1.StageSet {
	ss := newStageSet(ns, name, false, metav1.ConditionTrue, "Succeeded", "ok")
	ss.Spec.Stages = []stagesv1.Stage{{Name: stage}}
	ss.Status.LastAppliedSnapshot = []stagesv1.StageArtifactRef{{Stage: stage, URL: "u", Digest: applied, Revision: "r"}}
	return ss
}

// TestDiffRevisions_StageSetGetError covers the apiserver-failure branch of
// diffRevisionsHandler where the StageSet Get itself fails.
func TestDiffRevisions_StageSetGetError(t *testing.T) {
	const ns, name, stage = "team-a", "web", "prod"
	c := failingClient(t, interceptor.Funcs{
		Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
			return errors.New("apiserver down")
		},
	})
	cfg := Config{KubeClient: c, RollbackStore: &fakeRollback{data: map[string][]byte{}}}

	res, _, err := cfg.diffRevisionsHandler(context.Background(), nil,
		diffRevisionsInput{Namespace: ns, Name: name, Stage: stage, From: "sha256:aaaa", To: "sha256:bbbb"})
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("expected a tool error when the StageSet Get fails, got %+v", res)
	}
	if !strings.Contains(textContent(t, res), "cannot get StageSet") {
		t.Fatalf("error should name the failed Get, got %q", textContent(t, res))
	}
}

// TestDiffRevisions_ToReadError covers the second readSnapshot call's error
// branch: 'from' decodes cleanly but 'to' is absent from the store, exercising
// the snapshotReadError("to", ...) path.
func TestDiffRevisions_ToReadError(t *testing.T) {
	const ns, name, stage = "team-a", "web", "prod"
	from, to := "sha256:aaaa", "sha256:bbbb"
	store := &fakeRollback{data: map[string][]byte{
		rollbackstore.Key(ns, name, stage, from): mustEncode(t, deployment("web", 1)), // 'to' absent
	}}
	cfg := Config{KubeClient: fakeClient(t, diffStageSet(ns, name, stage, to)), RollbackStore: store}

	res, _, err := cfg.diffRevisionsHandler(context.Background(), nil,
		diffRevisionsInput{Namespace: ns, Name: name, Stage: stage, From: from, To: to})
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("expected a tool error for the missing 'to' revision, got %+v", res)
	}
	if !strings.Contains(textContent(t, res), "to revision") {
		t.Fatalf("error should reference the 'to' revision, got %q", textContent(t, res))
	}
}

// TestReadSnapshot_StoreError covers readSnapshot's store-Get failure branch and,
// through it, snapshotReadError's generic (non-missing) message.
func TestReadSnapshot_StoreError(t *testing.T) {
	const ns, name, stage = "team-a", "web", "prod"
	from, to := "sha256:aaaa", "sha256:bbbb"
	cfg := Config{
		KubeClient:    fakeClient(t, diffStageSet(ns, name, stage, to)),
		RollbackStore: errRollback{err: errors.New("s3 timeout")},
	}

	res, _, err := cfg.diffRevisionsHandler(context.Background(), nil,
		diffRevisionsInput{Namespace: ns, Name: name, Stage: stage, From: from, To: to})
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("expected a tool error when the store read fails, got %+v", res)
	}
	msg := textContent(t, res)
	if !strings.Contains(msg, "cannot read from revision") || !strings.Contains(msg, "s3 timeout") {
		t.Fatalf("error should surface the underlying store failure, got %q", msg)
	}
}

// TestReadSnapshot_SnapshotTooLarge covers the size-cap branch of readSnapshot:
// a stored snapshot above maxDiffSnapshotBytes is refused before decoding.
func TestReadSnapshot_SnapshotTooLarge(t *testing.T) {
	const ns, name, stage = "team-a", "web", "prod"
	from, to := "sha256:aaaa", "sha256:bbbb"
	oversize := make([]byte, maxDiffSnapshotBytes+1)
	store := &fakeRollback{data: map[string][]byte{
		rollbackstore.Key(ns, name, stage, from): oversize,
		rollbackstore.Key(ns, name, stage, to):   mustEncode(t, deployment("web", 1)),
	}}
	cfg := Config{KubeClient: fakeClient(t, diffStageSet(ns, name, stage, to)), RollbackStore: store}

	res, _, err := cfg.diffRevisionsHandler(context.Background(), nil,
		diffRevisionsInput{Namespace: ns, Name: name, Stage: stage, From: from, To: to})
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("expected a tool error for an oversized snapshot, got %+v", res)
	}
	if !strings.Contains(textContent(t, res), "too large to diff") {
		t.Fatalf("error should mention the size cap, got %q", textContent(t, res))
	}
}

// TestReadSnapshot_MalformedJSON covers readSnapshot's json.Unmarshal error
// branch: a stored snapshot that is not a JSON array of objects.
func TestReadSnapshot_MalformedJSON(t *testing.T) {
	const ns, name, stage = "team-a", "web", "prod"
	from, to := "sha256:aaaa", "sha256:bbbb"
	store := &fakeRollback{data: map[string][]byte{
		rollbackstore.Key(ns, name, stage, from): []byte("{not valid json"),
		rollbackstore.Key(ns, name, stage, to):   mustEncode(t, deployment("web", 1)),
	}}
	cfg := Config{KubeClient: fakeClient(t, diffStageSet(ns, name, stage, to)), RollbackStore: store}

	res, _, err := cfg.diffRevisionsHandler(context.Background(), nil,
		diffRevisionsInput{Namespace: ns, Name: name, Stage: stage, From: from, To: to})
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("expected a tool error for a malformed snapshot, got %+v", res)
	}
	if !strings.Contains(textContent(t, res), "decode snapshot") {
		t.Fatalf("error should mention the decode failure, got %q", textContent(t, res))
	}
}

// TestChangesBetween_SortComparatorOrdersByGVKThenNamespaceThenName exercises the
// full ordering comparator in changesBetween: objects that differ by GVK, by
// namespace within a GVK, and by name within a namespace, so every comparison
// branch is taken and the output stays deterministically sorted.
func TestChangesBetween_SortComparatorOrdersByGVKThenNamespaceThenName(t *testing.T) {
	obj := func(group, kind, ns, name string) map[string]any {
		return map[string]any{
			"apiVersion": group + "/v1",
			"kind":       kind,
			"metadata":   map[string]any{"name": name, "namespace": ns},
		}
	}
	// All objects only exist in 'to' (all creates), deliberately fed in an order
	// that is not the sorted order, so a stable sort must reorder them.
	to := []map[string]any{
		obj("apps", "Deployment", "team-b", "z"),
		obj("apps", "Deployment", "team-a", "b"),
		obj("apps", "Deployment", "team-a", "a"),
		obj("batch", "CronJob", "team-a", "a"),
	}
	const ns, name, stage = "team-a", "web", "prod"
	from, toDigest := "sha256:aaaa", "sha256:bbbb"
	store := &fakeRollback{data: map[string][]byte{
		rollbackstore.Key(ns, name, stage, from):     mustEncode(t), // empty 'from' → all creates
		rollbackstore.Key(ns, name, stage, toDigest): mustEncode(t, to...),
	}}
	cfg := Config{KubeClient: fakeClient(t, diffStageSet(ns, name, stage, toDigest)), RollbackStore: store}

	res, out, err := cfg.diffRevisionsHandler(context.Background(), nil,
		diffRevisionsInput{Namespace: ns, Name: name, Stage: stage, From: from, To: toDigest})
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if res != nil && res.IsError {
		t.Fatalf("unexpected tool error: %s", textContent(t, res))
	}
	if out.Create != 4 || out.Configure != 0 || out.Delete != 0 || out.Unchanged != 0 {
		t.Fatalf("want 4 creates, got create=%d configure=%d delete=%d unchanged=%d",
			out.Create, out.Configure, out.Delete, out.Unchanged)
	}
	// apps/Deployment sorts before batch/CronJob; within apps, team-a before
	// team-b; within team-a, name a before b. The per-object headers carry the
	// GVK/namespace/name, so their order in the rendered diff reflects the sort.
	order := []string{
		"Deployment/a [team-a]",
		"Deployment/b [team-a]",
		"Deployment/z [team-b]",
		"CronJob/a [team-a]",
	}
	prev := -1
	for _, header := range order {
		idx := strings.Index(out.Diff, header)
		if idx == -1 {
			t.Fatalf("expected %q in the diff:\n%s", header, out.Diff)
		}
		if idx <= prev {
			t.Fatalf("diff objects out of sorted order at %q:\n%s", header, out.Diff)
		}
		prev = idx
	}
}
