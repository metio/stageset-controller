/*
 * SPDX-FileCopyrightText: The stageset-controller Authors
 * SPDX-License-Identifier: 0BSD
 */

package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/rollbackstore"
)

// fakeRollback is an in-memory RollbackReader keyed exactly like the real store.
type fakeRollback struct{ data map[string][]byte }

func (f *fakeRollback) Get(_ context.Context, key string) ([]byte, bool, error) {
	d, ok := f.data[key]
	return d, ok, nil
}

func deployment(name string, replicas int) map[string]any {
	return map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]any{"name": name, "namespace": "team-a"},
		"spec":       map[string]any{"replicas": int64(replicas)},
	}
}

func mustEncode(t *testing.T, objs ...map[string]any) []byte {
	t.Helper()
	data, err := json.Marshal(objs)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return data
}

func TestDiffRevisionsHandler(t *testing.T) {
	const ns, name, stage = "team-a", "web", "prod"
	from, to := "sha256:aaaa", "sha256:bbbb"

	// A StageSet whose prod stage currently has 'to' applied — so an omitted
	// 'to' defaults to it.
	ssWithApplied := func() *stagesv1.StageSet {
		ss := newStageSet(ns, name, false, metav1.ConditionTrue, "Succeeded", "ok")
		ss.Spec.Stages = []stagesv1.Stage{{Name: stage}}
		ss.Status.LastAppliedSnapshot = []stagesv1.StageArtifactRef{{Stage: stage, URL: "u", Digest: to, Revision: "r"}}
		return ss
	}

	t.Run("modified object, to defaults to the applied snapshot digest", func(t *testing.T) {
		store := &fakeRollback{data: map[string][]byte{
			rollbackstore.Key(ns, name, stage, from): mustEncode(t, deployment("web", 1)),
			rollbackstore.Key(ns, name, stage, to):   mustEncode(t, deployment("web", 2)),
		}}
		cfg := Config{KubeClient: fakeClient(t, ssWithApplied()), RollbackStore: store}

		res, out, err := cfg.diffRevisionsHandler(context.Background(), nil, diffRevisionsInput{Namespace: ns, Name: name, Stage: stage, From: from})
		if err != nil {
			t.Fatalf("handler error: %v", err)
		}
		if res != nil {
			t.Fatalf("unexpected tool error: %+v", res)
		}
		if out.To != to {
			t.Fatalf("to should default to the applied digest %q, got %q", to, out.To)
		}
		if out.Configure != 1 || out.Create != 0 || out.Delete != 0 {
			t.Fatalf("want 1 configure, got create=%d configure=%d delete=%d", out.Create, out.Configure, out.Delete)
		}
		if !strings.Contains(out.Diff, "replicas") {
			t.Fatalf("diff should mention the changed field:\n%s", out.Diff)
		}
	})

	t.Run("create, delete, and unchanged with explicit revisions", func(t *testing.T) {
		store := &fakeRollback{data: map[string][]byte{
			rollbackstore.Key(ns, name, stage, from): mustEncode(t, deployment("gone", 1), deployment("same", 3)),
			rollbackstore.Key(ns, name, stage, to):   mustEncode(t, deployment("new", 1), deployment("same", 3)),
		}}
		cfg := Config{KubeClient: fakeClient(t, ssWithApplied()), RollbackStore: store}

		_, out, err := cfg.diffRevisionsHandler(context.Background(), nil, diffRevisionsInput{Namespace: ns, Name: name, Stage: stage, From: from, To: to})
		if err != nil {
			t.Fatalf("handler error: %v", err)
		}
		if out.Create != 1 || out.Delete != 1 || out.Unchanged != 1 {
			t.Fatalf("want create=1 delete=1 unchanged=1, got create=%d delete=%d unchanged=%d", out.Create, out.Delete, out.Unchanged)
		}
	})

	t.Run("a revision missing from the store is a tool error", func(t *testing.T) {
		store := &fakeRollback{data: map[string][]byte{
			rollbackstore.Key(ns, name, stage, to): mustEncode(t, deployment("web", 2)), // 'from' absent
		}}
		cfg := Config{KubeClient: fakeClient(t, ssWithApplied()), RollbackStore: store}

		res, _, err := cfg.diffRevisionsHandler(context.Background(), nil, diffRevisionsInput{Namespace: ns, Name: name, Stage: stage, From: from})
		if err != nil {
			t.Fatalf("handler error: %v", err)
		}
		if res == nil || !res.IsError {
			t.Fatalf("expected a tool error for the missing 'from' revision, got %+v", res)
		}
	})

	t.Run("omitted to with no applied snapshot is a tool error", func(t *testing.T) {
		store := &fakeRollback{data: map[string][]byte{}}
		ss := newStageSet(ns, name, false, metav1.ConditionTrue, "Succeeded", "ok")
		ss.Spec.Stages = []stagesv1.Stage{{Name: stage}}
		cfg := Config{KubeClient: fakeClient(t, ss), RollbackStore: store}

		res, _, _ := cfg.diffRevisionsHandler(context.Background(), nil, diffRevisionsInput{Namespace: ns, Name: name, Stage: stage, From: from})
		if res == nil || !res.IsError {
			t.Fatalf("expected a tool error when 'to' can't be defaulted, got %+v", res)
		}
	})

	t.Run("an undeclared stage is a tool error", func(t *testing.T) {
		cfg := Config{KubeClient: fakeClient(t, ssWithApplied()), RollbackStore: &fakeRollback{data: map[string][]byte{}}}
		res, _, _ := cfg.diffRevisionsHandler(context.Background(), nil, diffRevisionsInput{Namespace: ns, Name: name, Stage: "ghost", From: from})
		if res == nil || !res.IsError {
			t.Fatalf("expected a tool error for a stage the StageSet does not declare, got %+v", res)
		}
	})

	t.Run("a path-bearing digest is rejected before any store read", func(t *testing.T) {
		cfg := Config{KubeClient: fakeClient(t, ssWithApplied()), RollbackStore: &fakeRollback{data: map[string][]byte{}}}
		// from with a traversal value: on the S3 backend this would otherwise
		// become part of the object name and escape the key prefix.
		res, _, _ := cfg.diffRevisionsHandler(context.Background(), nil,
			diffRevisionsInput{Namespace: ns, Name: name, Stage: stage, From: "sha256:../../../etc/passwd"})
		if res == nil || !res.IsError {
			t.Fatalf("expected a tool error for a path-bearing from digest, got %+v", res)
		}
		// An explicit path-bearing to is likewise rejected.
		res, _, _ = cfg.diffRevisionsHandler(context.Background(), nil,
			diffRevisionsInput{Namespace: ns, Name: name, Stage: stage, From: from, To: "sha256:a/b"})
		if res == nil || !res.IsError {
			t.Fatalf("expected a tool error for a path-bearing to digest, got %+v", res)
		}
	})

	t.Run("missing required inputs are tool errors", func(t *testing.T) {
		cfg := Config{KubeClient: fakeClient(t), RollbackStore: &fakeRollback{data: map[string][]byte{}}}
		for _, in := range []diffRevisionsInput{
			{Namespace: ns, Name: name, From: from},   // no stage
			{Namespace: ns, Name: name, Stage: stage}, // no from
			{Name: name, Stage: stage, From: from},    // no namespace
		} {
			res, _, _ := cfg.diffRevisionsHandler(context.Background(), nil, in)
			if res == nil || !res.IsError {
				t.Fatalf("expected a tool error for input %+v, got %+v", in, res)
			}
		}
	})
}

func TestDiffTool_RegisteredOnlyWithRollbackStore(t *testing.T) {
	if registeredTools(t, Config{KubeClient: fakeClient(t)})["diff_revisions"] {
		t.Fatal("diff_revisions must not be registered without a RollbackStore")
	}
	withStore := Config{KubeClient: fakeClient(t), RollbackStore: &fakeRollback{data: map[string][]byte{}}}
	if !registeredTools(t, withStore)["diff_revisions"] {
		t.Fatal("diff_revisions must be registered when a RollbackStore is configured")
	}
}

// registeredTools connects an in-memory client to a server built from cfg and
// returns the set of advertised tool names.
func registeredTools(t *testing.T, cfg Config) map[string]bool {
	t.Helper()
	ctx := context.Background()
	server := NewServer(cfg)
	clientT, serverT := mcpsdk.NewInMemoryTransports()
	ss, err := server.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	defer func() { _ = ss.Close() }()
	c := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test-client", Version: "test"}, nil)
	cs, err := c.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer func() { _ = cs.Close() }()
	lt, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	present := map[string]bool{}
	for _, tool := range lt.Tools {
		present[tool.Name] = true
	}
	return present
}
