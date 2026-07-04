/*
 * SPDX-FileCopyrightText: The stageset-controller Authors
 * SPDX-License-Identifier: 0BSD
 */

package mcp

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"pgregory.net/rapid"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

// failingClient builds a fake client whose calls are intercepted by funcs, so a
// test can force List/Patch errors that the in-memory fake never produces.
func failingClient(t *testing.T, funcs interceptor.Funcs, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := stagesv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).WithInterceptorFuncs(funcs).Build()
}

// TestNewHTTPHandler_ServesOverHTTP drives the tools through the real streamable
// HTTP transport (the in-cluster path), covering NewHTTPHandler end to end.
func TestNewHTTPHandler_ServesOverHTTP(t *testing.T) {
	ctx := context.Background()
	cfg := Config{
		Version:        "test",
		KubeClient:     fakeClient(t, newStageSet("team-a", "web", false, metav1.ConditionTrue, "Succeeded", "ok")),
		RunbookBaseURL: testRunbookBase,
		AllowMutations: true,
	}
	srv := httptest.NewServer(NewHTTPHandler(cfg))
	defer srv.Close()

	c := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test-client", Version: "test"}, nil)
	cs, err := c.Connect(ctx, &mcpsdk.StreamableClientTransport{Endpoint: srv.URL}, nil)
	if err != nil {
		t.Fatalf("connect over HTTP: %v", err)
	}
	defer func() { _ = cs.Close() }()

	lt, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(lt.Tools) == 0 {
		t.Fatal("no tools advertised over HTTP transport")
	}

	res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "get_stageset",
		Arguments: map[string]any{"namespace": "team-a", "name": "web"},
	})
	if err != nil {
		t.Fatalf("call get_stageset over HTTP: %v", err)
	}
	if res.IsError {
		t.Fatalf("get_stageset tool error: %s", textContent(t, res))
	}
}

func TestListStageSets_ClientError(t *testing.T) {
	c := failingClient(t, interceptor.Funcs{
		List: func(_ context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
			return errors.New("apiserver down")
		},
	})
	res, _, err := Config{KubeClient: c}.listStageSetsHandler(context.Background(), nil, listStageSetsInput{})
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("expected IsError on List failure, got %+v", res)
	}
}

// TestListStageSets_ClusterWideForbiddenHints pins that a Forbidden on the
// cluster-wide list (a namespace-scoped controller SA) returns a hint to pass an
// explicit namespace rather than a bare denial.
func TestListStageSets_ClusterWideForbiddenHints(t *testing.T) {
	forbidden := apierrors.NewForbidden(stagesv1.GroupVersion.WithResource("stagesets").GroupResource(), "", nil)
	c := failingClient(t, interceptor.Funcs{
		List: func(_ context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
			return forbidden
		},
	})
	res, _, err := Config{KubeClient: c}.listStageSetsHandler(context.Background(), nil, listStageSetsInput{})
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("expected IsError, got %+v", res)
	}
	if !strings.Contains(textContent(t, res), "pass an explicit namespace") {
		t.Fatalf("error should hint at an explicit namespace, got %q", textContent(t, res))
	}
}

func TestMutate_PatchError(t *testing.T) {
	c := failingClient(t, interceptor.Funcs{
		Patch: func(_ context.Context, _ client.WithWatch, _ client.Object, _ client.Patch, _ ...client.PatchOption) error {
			return errors.New("conflict storm")
		},
	}, newStageSet("team-a", "web", false, metav1.ConditionTrue, "Succeeded", "ok"))
	res, _, err := Config{KubeClient: c, AllowMutations: true}.suspendStageSetHandler(context.Background(), nil, mutateInput{Namespace: "team-a", Name: "web"})
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("expected IsError on Patch failure, got %+v", res)
	}
}

// TestRunbookURL_Property pins the runbook-link contract for arbitrary reasons:
// a non-empty reason maps to base + lowercase(reason) + "/", and an empty reason
// (or empty base) maps to "".
func TestRunbookURL_Property(t *testing.T) {
	cfg := Config{RunbookBaseURL: testRunbookBase}
	rapid.Check(t, func(rt *rapid.T) {
		reason := rapid.String().Draw(rt, "reason")
		got := cfg.runbookURL(reason)
		if reason == "" {
			if got != "" {
				rt.Fatalf("empty reason yielded %q", got)
			}
			return
		}
		want := testRunbookBase + strings.ToLower(reason) + "/"
		if got != want {
			rt.Fatalf("runbookURL(%q) = %q, want %q", reason, got, want)
		}
		if !strings.HasPrefix(got, testRunbookBase) || !strings.HasSuffix(got, "/") {
			rt.Fatalf("malformed runbook URL %q", got)
		}
	})
}

// The streamable-HTTP handler must bound idle sessions: with the SDK's zero
// SessionTimeout, a session created by an initialize POST is retained until an
// explicit DELETE, so a dropped/looping client on the unauthenticated port
// accumulates sessions until the pod OOMs. This pins the idle bound is set.
func TestMCPSessionIdleTimeout_IsBounded(t *testing.T) {
	if mcpSessionIdleTimeout <= 0 {
		t.Fatal("mcpSessionIdleTimeout must be positive; a zero timeout never expires idle sessions")
	}
	if mcpSessionIdleTimeout > time.Hour {
		t.Errorf("mcpSessionIdleTimeout = %s; an unbounded-in-practice idle window defeats the leak guard", mcpSessionIdleTimeout)
	}
}
