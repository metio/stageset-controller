// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package actions

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

func hostOf(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u.Hostname()
}

func TestAllowedURL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		hosts []string
		url   string
		ok    bool
	}{
		{"loopback denied without allowlist", nil, "http://127.0.0.1/x", false},
		{"loopback allowed when listed", []string{"127.0.0.1"}, "http://127.0.0.1/x", true},
		{"public allowed without allowlist", nil, "https://events.pagerduty.com/v2/enqueue", true},
		{"not in allowlist", []string{"hooks.slack.com"}, "https://evil.example/x", false},
		{"glob allowlist", []string{"*.slack.com"}, "https://hooks.slack.com/x", true},
		{"bad scheme", nil, "file:///etc/passwd", false},
		{"cloud metadata denied", nil, "http://169.254.169.254/latest/meta-data", false},
		{"localhost denied", nil, "http://localhost/x", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := &Executor{AllowedHosts: tc.hosts}
			if err := e.allowedURL(tc.url); (err == nil) != tc.ok {
				t.Fatalf("allowedURL(%q) err=%v, want ok=%v", tc.url, err, tc.ok)
			}
		})
	}
}

func TestRun_HTTPAction(t *testing.T) {
	t.Parallel()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	e := &Executor{HTTPClient: http.DefaultClient, AllowedHosts: []string{hostOf(t, srv.URL)}}
	var ran []string
	err := e.Run(context.Background(), "ns",
		[]stagesv1.Action{{Name: "ping", HTTP: &stagesv1.HTTPAction{URL: srv.URL}}},
		nil, func(n string) error { ran = append(ran, n); return nil })
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if atomic.LoadInt32(&hits) != 1 || len(ran) != 1 || ran[0] != "ping" {
		t.Fatalf("hits=%d ran=%v", hits, ran)
	}
}

func TestRun_HTTPUnexpectedStatus(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	e := &Executor{HTTPClient: http.DefaultClient, AllowedHosts: []string{hostOf(t, srv.URL)}}
	err := e.Run(context.Background(), "ns",
		[]stagesv1.Action{{Name: "ping", HTTP: &stagesv1.HTTPAction{URL: srv.URL}}}, nil, nil)
	if err == nil {
		t.Fatal("a 500 with no expectedStatus should fail")
	}
}

func TestRun_HTTPForbiddenHost(t *testing.T) {
	t.Parallel()
	e := &Executor{} // no allowlist → loopback denied
	err := e.Run(context.Background(), "ns",
		[]stagesv1.Action{{Name: "ping", HTTP: &stagesv1.HTTPAction{URL: "http://127.0.0.1:1/x"}}}, nil, nil)
	if !errors.Is(err, ErrForbiddenHost) {
		t.Fatalf("want ErrForbiddenHost, got %v", err)
	}
}

func TestRun_WaitDuration(t *testing.T) {
	t.Parallel()
	e := &Executor{}
	start := time.Now()
	err := e.Run(context.Background(), "ns",
		[]stagesv1.Action{{Name: "settle", Wait: &stagesv1.WaitAction{Duration: &metav1.Duration{Duration: 20 * time.Millisecond}}}}, nil, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if time.Since(start) < 15*time.Millisecond {
		t.Fatal("wait returned too early")
	}
}

func TestRun_JobFailsClosedWithoutResolver(t *testing.T) {
	t.Parallel()
	e := &Executor{} // no resolver/fetcher
	err := e.Run(context.Background(), "ns",
		[]stagesv1.Action{{Name: "smoke", Job: &stagesv1.JobAction{}}}, nil, nil)
	if !errors.Is(err, ErrActionUnsupported) {
		t.Fatalf("job without a resolver: want ErrActionUnsupported, got %v", err)
	}
}

func TestRun_WaitExprRequiresTarget(t *testing.T) {
	t.Parallel()
	e := &Executor{}
	err := e.Run(context.Background(), "ns",
		[]stagesv1.Action{{Name: "drain", Wait: &stagesv1.WaitAction{Expr: "status.x == 0"}}}, nil, nil)
	if err == nil {
		t.Fatal("a wait with an expr but no target should error")
	}
}

func TestRun_ApplyFailsClosedWithoutApplier(t *testing.T) {
	t.Parallel()
	e := &Executor{} // no resolver/fetcher/applier
	err := e.Run(context.Background(), "ns",
		[]stagesv1.Action{{Name: "maint-up", Apply: &stagesv1.ApplyAction{}}}, nil, nil)
	if !errors.Is(err, ErrActionUnsupported) {
		t.Fatalf("apply without an applier: want ErrActionUnsupported, got %v", err)
	}
}

func jobUnstructured(ns, name, condType, condStatus string) *unstructured.Unstructured {
	o := &unstructured.Unstructured{}
	o.SetGroupVersionKind(schema.GroupVersionKind{Group: "batch", Version: "v1", Kind: "Job"})
	o.SetNamespace(ns)
	o.SetName(name)
	_ = unstructured.SetNestedSlice(o.Object, []any{map[string]any{"type": condType, "status": condStatus}}, "status", "conditions")
	return o
}

func jobClient(t *testing.T, objs ...*unstructured.Unstructured) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	gvk := schema.GroupVersionKind{Group: "batch", Version: "v1", Kind: "Job"}
	s.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
	list := gvk
	list.Kind += "List"
	s.AddKnownTypeWithName(list, &unstructured.UnstructuredList{})
	b := fake.NewClientBuilder().WithScheme(s)
	for _, o := range objs {
		b = b.WithObjects(o)
	}
	return b.Build()
}

func TestAwaitJob(t *testing.T) {
	t.Parallel()
	complete := jobUnstructured("ns", "ok", "Complete", "True")
	e := &Executor{Client: jobClient(t, complete)}
	if err := e.awaitJob(context.Background(), complete); err != nil {
		t.Fatalf("completed job should return nil, got %v", err)
	}

	failed := jobUnstructured("ns", "bad", "Failed", "True")
	e2 := &Executor{Client: jobClient(t, failed)}
	if err := e2.awaitJob(context.Background(), failed); err == nil {
		t.Fatal("failed job should return an error")
	}
}

func TestRun_SkipsDone(t *testing.T) {
	t.Parallel()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	defer srv.Close()

	e := &Executor{HTTPClient: http.DefaultClient, AllowedHosts: []string{hostOf(t, srv.URL)}}
	acts := []stagesv1.Action{{Name: "ping", HTTP: &stagesv1.HTTPAction{URL: srv.URL}}}
	if err := e.Run(context.Background(), "ns", acts, map[string]bool{"ping": true}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if atomic.LoadInt32(&hits) != 0 {
		t.Fatalf("ledgered action should not fire, hits=%d", hits)
	}
}

func TestStatusAccepted(t *testing.T) {
	t.Parallel()
	if !statusAccepted(204, nil) || statusAccepted(500, nil) {
		t.Fatal("default 2xx acceptance wrong")
	}
	if !statusAccepted(418, []int32{418}) || statusAccepted(200, []int32{418}) {
		t.Fatal("explicit expectedStatus matching wrong")
	}
}
