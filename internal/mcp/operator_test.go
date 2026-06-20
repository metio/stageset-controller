/*
 * SPDX-FileCopyrightText: The stageset-controller Authors
 * SPDX-License-Identifier: 0BSD
 */

package mcp

import (
	"context"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

const testRunbookBase = "https://stageset.projects.metio.wtf/runbooks/"

func newStageSet(namespace, name string, suspend bool, ready metav1.ConditionStatus, reason, message string) *stagesv1.StageSet {
	return &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec:       stagesv1.StageSetSpec{Suspend: suspend},
		Status: stagesv1.StageSetStatus{
			ObservedGeneration:   2,
			Version:              "1.2.3",
			LastAppliedRevisions: map[string]string{"deploy": "rev-abc"},
			Conditions: []metav1.Condition{{
				Type:    conditionReady,
				Status:  ready,
				Reason:  reason,
				Message: message,
			}},
			Stages: []stagesv1.StageStatus{
				{Name: "deploy", Phase: stagesv1.StagePhase("Ready"), AppliedRevision: "rev-abc", Message: "applied"},
			},
		},
	}
}

func fakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := stagesv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func textContent(t *testing.T, res *mcpsdk.CallToolResult) string {
	t.Helper()
	if res == nil || len(res.Content) == 0 {
		t.Fatalf("result has no content: %+v", res)
	}
	tc, ok := res.Content[0].(*mcpsdk.TextContent)
	if !ok {
		t.Fatalf("first content block is %T, want *TextContent", res.Content[0])
	}
	return tc.Text
}

func TestListStageSetsHandler(t *testing.T) {
	cfg := Config{KubeClient: fakeClient(
		t,
		newStageSet("team-a", "web", false, metav1.ConditionTrue, "Succeeded", "ok"),
		newStageSet("team-b", "api", true, metav1.ConditionFalse, "Suspended", "paused"),
	)}

	t.Run("all namespaces", func(t *testing.T) {
		res, out, err := cfg.listStageSetsHandler(context.Background(), nil, listStageSetsInput{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res != nil && res.IsError {
			t.Fatalf("unexpected tool error: %s", textContent(t, res))
		}
		if len(out.StageSets) != 2 {
			t.Fatalf("got %d, want 2: %+v", len(out.StageSets), out.StageSets)
		}
	})

	t.Run("scoped + fields", func(t *testing.T) {
		_, out, err := cfg.listStageSetsHandler(context.Background(), nil, listStageSetsInput{Namespace: "team-a"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(out.StageSets) != 1 {
			t.Fatalf("want 1, got %+v", out.StageSets)
		}
		s := out.StageSets[0]
		if s.Name != "web" || s.Ready != "True" || s.Reason != "Succeeded" || s.Suspended || s.Version != "1.2.3" {
			t.Fatalf("summary wrong: %+v", s)
		}
	})

	t.Run("empty namespace yields empty slice not nil", func(t *testing.T) {
		_, out, err := cfg.listStageSetsHandler(context.Background(), nil, listStageSetsInput{Namespace: "nope"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if out.StageSets == nil {
			t.Fatal("StageSets is nil, want empty slice")
		}
	})
}

func TestGetStageSetHandler(t *testing.T) {
	ss := newStageSet("team-a", "web", false, metav1.ConditionFalse, "RBACDenied", "the tenant SA cannot apply")
	ss.Status.PendingMigrations = []string{"m1"}
	cfg := Config{KubeClient: fakeClient(t, ss), RunbookBaseURL: testRunbookBase}

	t.Run("full detail with runbook link", func(t *testing.T) {
		res, out, err := cfg.getStageSetHandler(context.Background(), nil, getStageSetInput{Namespace: "team-a", Name: "web"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res != nil && res.IsError {
			t.Fatalf("unexpected tool error: %s", textContent(t, res))
		}
		if out.Ready != "False" || out.Reason != "RBACDenied" {
			t.Fatalf("ready/reason wrong: %+v", out)
		}
		if want := testRunbookBase + "rbacdenied/"; out.RunbookURL != want {
			t.Fatalf("RunbookURL = %q, want %q", out.RunbookURL, want)
		}
		if len(out.Stages) != 1 || out.Stages[0].Phase != "Ready" || out.Stages[0].Name != "deploy" {
			t.Fatalf("stages wrong: %+v", out.Stages)
		}
		if out.LastAppliedRevisions["deploy"] != "rev-abc" {
			t.Fatalf("revisions wrong: %+v", out.LastAppliedRevisions)
		}
		if len(out.PendingMigrations) != 1 {
			t.Fatalf("pending migrations wrong: %+v", out.PendingMigrations)
		}
	})

	t.Run("not found is a tool error", func(t *testing.T) {
		res, _, err := cfg.getStageSetHandler(context.Background(), nil, getStageSetInput{Namespace: "team-a", Name: "missing"})
		if err != nil {
			t.Fatalf("unexpected Go error: %v", err)
		}
		if res == nil || !res.IsError {
			t.Fatalf("expected IsError, got %+v", res)
		}
	})

	t.Run("missing namespace or name is a tool error", func(t *testing.T) {
		res, _, _ := cfg.getStageSetHandler(context.Background(), nil, getStageSetInput{Name: "web"})
		if res == nil || !res.IsError {
			t.Fatalf("expected IsError when namespace empty, got %+v", res)
		}
	})
}

func TestReadyCondition_NoConditionIsUnknown(t *testing.T) {
	status, reason, message := readyCondition(&stagesv1.StageSet{})
	if status != "Unknown" || reason != "" || message != "" {
		t.Fatalf("got (%q,%q,%q), want (Unknown,,)", status, reason, message)
	}
}

func TestRunbookURL(t *testing.T) {
	cfg := Config{RunbookBaseURL: testRunbookBase}
	if got := cfg.runbookURL("StageFailed"); got != testRunbookBase+"stagefailed/" {
		t.Fatalf("runbookURL = %q", got)
	}
	if got := cfg.runbookURL(""); got != "" {
		t.Fatalf("empty reason should yield empty URL, got %q", got)
	}
	if got := (Config{}).runbookURL("StageFailed"); got != "" {
		t.Fatalf("no base URL should yield empty URL, got %q", got)
	}
}

func TestServer_InMemoryRoundTrip(t *testing.T) {
	ctx := context.Background()
	cfg := Config{
		Version:        "test",
		KubeClient:     fakeClient(t, newStageSet("team-a", "web", false, metav1.ConditionTrue, "Succeeded", "ok")),
		RunbookBaseURL: testRunbookBase,
	}
	cs := connectClient(t, ctx, NewServer(cfg))

	lt, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	got := map[string]bool{}
	for _, tool := range lt.Tools {
		got[tool.Name] = true
	}
	for _, want := range []string{"list_stagesets", "get_stageset"} {
		if !got[want] {
			t.Errorf("tool %q not registered; have %v", want, got)
		}
	}

	res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "get_stageset",
		Arguments: map[string]any{"namespace": "team-a", "name": "web"},
	})
	if err != nil {
		t.Fatalf("call get_stageset: %v", err)
	}
	if res.IsError {
		t.Fatalf("get_stageset tool error: %s", textContent(t, res))
	}
	sc, ok := res.StructuredContent.(map[string]any)
	if !ok || sc["ready"] != "True" {
		t.Fatalf("unexpected structured content: %v", res.StructuredContent)
	}
}

// connectClient wires an in-memory client+server session and returns the client
// session, registering cleanup.
func connectClient(t *testing.T, ctx context.Context, server *mcpsdk.Server) *mcpsdk.ClientSession {
	t.Helper()
	clientT, serverT := mcpsdk.NewInMemoryTransports()
	ss, err := server.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	c := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test-client", Version: "test"}, nil)
	cs, err := c.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func TestOperatorTools_RequireClient(t *testing.T) {
	ctx := context.Background()
	cs := connectClient(t, ctx, NewServer(Config{Version: "test"})) // no client

	lt, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(lt.Tools) != 0 {
		t.Fatalf("expected no tools without a client, got %d", len(lt.Tools))
	}
}
