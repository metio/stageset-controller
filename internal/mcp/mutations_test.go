/*
 * SPDX-FileCopyrightText: The stageset-controller Authors
 * SPDX-License-Identifier: 0BSD
 */

package mcp

import (
	"context"
	"strings"
	"testing"

	fluxmeta "github.com/fluxcd/pkg/apis/meta"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

func getStageSet(t *testing.T, c client.Client, namespace, name string) *stagesv1.StageSet {
	t.Helper()
	var ss stagesv1.StageSet
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: name}, &ss); err != nil {
		t.Fatalf("re-get %s/%s: %v", namespace, name, err)
	}
	return &ss
}

func TestSuspendResumeHandlers(t *testing.T) {
	c := fakeClient(t, newStageSet("team-a", "web", false, metav1.ConditionTrue, "Succeeded", "ok"))
	cfg := Config{KubeClient: c, AllowMutations: true}
	in := mutateInput{Namespace: "team-a", Name: "web"}

	_, out, err := cfg.suspendStageSetHandler(context.Background(), nil, in)
	if err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if out.Result != "suspended" {
		t.Fatalf("suspend result = %q", out.Result)
	}
	if !getStageSet(t, c, "team-a", "web").Spec.Suspend {
		t.Fatal("spec.suspend not set")
	}

	_, out, _ = cfg.suspendStageSetHandler(context.Background(), nil, in)
	if out.Result != "already suspended" {
		t.Fatalf("re-suspend result = %q", out.Result)
	}

	_, out, err = cfg.resumeStageSetHandler(context.Background(), nil, in)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if out.Result != "resumed" {
		t.Fatalf("resume result = %q", out.Result)
	}
	if getStageSet(t, c, "team-a", "web").Spec.Suspend {
		t.Fatal("spec.suspend not cleared")
	}

	_, out, _ = cfg.resumeStageSetHandler(context.Background(), nil, in)
	if out.Result != "not suspended; no change" {
		t.Fatalf("re-resume result = %q", out.Result)
	}
}

func TestReconcileStageSetHandler(t *testing.T) {
	c := fakeClient(t, newStageSet("team-a", "web", false, metav1.ConditionTrue, "Succeeded", "ok"))
	cfg := Config{KubeClient: c, AllowMutations: true}

	res, out, err := cfg.reconcileStageSetHandler(context.Background(), nil, mutateInput{Namespace: "team-a", Name: "web"})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res != nil && res.IsError {
		t.Fatalf("unexpected tool error: %s", textContent(t, res))
	}
	if !strings.HasPrefix(out.Result, "reconcile requested at ") {
		t.Fatalf("result = %q", out.Result)
	}
	if token := getStageSet(t, c, "team-a", "web").Annotations[fluxmeta.ReconcileRequestAnnotation]; token == "" {
		t.Fatalf("annotation %s not set", fluxmeta.ReconcileRequestAnnotation)
	}
}

func TestMutateStageSet_Errors(t *testing.T) {
	cfg := Config{KubeClient: fakeClient(t), AllowMutations: true}

	res, _, err := cfg.suspendStageSetHandler(context.Background(), nil, mutateInput{Namespace: "x", Name: "missing"})
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("expected IsError for missing, got %+v", res)
	}

	res, _, _ = cfg.reconcileStageSetHandler(context.Background(), nil, mutateInput{Name: "web"})
	if res == nil || !res.IsError {
		t.Fatalf("expected IsError when namespace empty, got %+v", res)
	}
}

func TestMutationTools_GatedByAllowMutations(t *testing.T) {
	mutationTools := []string{"reconcile_stageset", "suspend_stageset", "resume_stageset"}
	tests := []struct {
		name string
		cfg  Config
		want bool
	}{
		{"client + mutations on", Config{Version: "t", KubeClient: fakeClient(t), AllowMutations: true}, true},
		{"client + mutations off", Config{Version: "t", KubeClient: fakeClient(t), AllowMutations: false}, false},
		{"no client, mutations on", Config{Version: "t", AllowMutations: true}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			cs := connectClient(t, ctx, NewServer(tt.cfg))
			lt, err := cs.ListTools(ctx, nil)
			if err != nil {
				t.Fatalf("list tools: %v", err)
			}
			present := map[string]bool{}
			for _, tool := range lt.Tools {
				present[tool.Name] = true
			}
			for _, name := range mutationTools {
				if present[name] != tt.want {
					t.Errorf("tool %q present=%v, want %v", name, present[name], tt.want)
				}
			}
		})
	}
}

// TestCallMutationOverProtocol exercises a write tool through the real MCP
// protocol so the input-schema decoding and the patch path are covered together.
func TestCallMutationOverProtocol(t *testing.T) {
	ctx := context.Background()
	c := fakeClient(t, newStageSet("team-a", "web", false, metav1.ConditionTrue, "Succeeded", "ok"))
	cs := connectClient(t, ctx, NewServer(Config{Version: "test", KubeClient: c, AllowMutations: true}))

	res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "suspend_stageset",
		Arguments: map[string]any{"namespace": "team-a", "name": "web"},
	})
	if err != nil {
		t.Fatalf("call suspend_stageset: %v", err)
	}
	if res.IsError {
		t.Fatalf("suspend tool error: %s", textContent(t, res))
	}
	if !getStageSet(t, c, "team-a", "web").Spec.Suspend {
		t.Fatal("suspend over protocol did not persist")
	}
}
