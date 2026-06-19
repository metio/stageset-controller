// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package actions

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fluxcd/pkg/apis/meta"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	apitypes "k8s.io/apimachinery/pkg/types"
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

// TestHTTPAction_DialPinsResolvedIP proves the dial-time guard: a host on the
// allowlist that resolves to a forbidden address is rejected at connect, after
// the string-level allowedURL check has already passed.
func TestHTTPAction_DialPinsResolvedIP(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		resolve net.IP
	}{
		{"loopback", net.ParseIP("127.0.0.1")},
		{"link-local metadata", net.ParseIP("169.254.169.254")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := &Executor{
				AllowedHosts: []string{"internal.example"},
				lookupIP: func(_ context.Context, _ string) ([]net.IP, error) {
					return []net.IP{tc.resolve}, nil
				},
			}
			err := e.Run(context.Background(), "ns",
				[]stagesv1.Action{{Name: "ping", HTTP: &stagesv1.HTTPAction{URL: "http://internal.example/x"}}}, nil, nil)
			if !errors.Is(err, ErrForbiddenAddress) {
				t.Fatalf("want ErrForbiddenAddress, got %v", err)
			}
		})
	}
}

// TestHTTPAction_InetAtonRejectedAtDial proves an inet_aton-form literal that
// the libc resolver honors (2130706433 == 127.0.0.1) is rejected at dial: the
// resolver returns the loopback IP and the dial-time pin denies it.
func TestHTTPAction_InetAtonRejectedAtDial(t *testing.T) {
	t.Parallel()
	e := &Executor{
		lookupIP: func(_ context.Context, host string) ([]net.IP, error) {
			// The single-int form 2130706433 decodes to 127.0.0.1.
			if host == "2130706433" {
				return []net.IP{net.ParseIP("127.0.0.1")}, nil
			}
			return nil, errors.New("unexpected host " + host)
		},
	}
	err := e.Run(context.Background(), "ns",
		[]stagesv1.Action{{Name: "ping", HTTP: &stagesv1.HTTPAction{URL: "http://2130706433/"}}}, nil, nil)
	if !errors.Is(err, ErrForbiddenAddress) {
		t.Fatalf("want ErrForbiddenAddress, got %v", err)
	}
}

// TestHTTPAction_AllowedHostStillDials proves the dial guard does not break the
// happy path: a permitted host that resolves to the httptest listener connects.
func TestHTTPAction_AllowedHostStillDials(t *testing.T) {
	t.Parallel()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	srvHost := hostOf(t, srv.URL)
	e := &Executor{
		AllowedHosts: []string{srvHost},
		// Permit the loopback the httptest server listens on; production would
		// reject it, but the allowlist + this validator opt the test in.
		IPValidator: PermissiveIP,
		lookupIP: func(_ context.Context, host string) ([]net.IP, error) {
			return net.DefaultResolver.LookupIP(context.Background(), "ip", host)
		},
	}
	err := e.Run(context.Background(), "ns",
		[]stagesv1.Action{{Name: "ping", HTTP: &stagesv1.HTTPAction{URL: srv.URL}}}, nil, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("hits = %d, want 1", hits)
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

func TestShortHash(t *testing.T) {
	t.Parallel()
	h := shortHash("sha256:deadbeef")
	if len(h) != 8 {
		t.Fatalf("shortHash length = %d, want 8", len(h))
	}
	if shortHash("a") == shortHash("b") {
		t.Fatal("distinct inputs must hash differently")
	}
	first, second := shortHash("revision-x"), shortHash("revision-x")
	if first != second {
		t.Fatalf("shortHash must be deterministic: %q != %q", first, second)
	}
}

func TestSuffixName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, suffix, want string
	}{
		{"short", "-abc", "short-abc"},
		// 60-char base + 4-char suffix would be 64 > 63, so the base is
		// truncated to 59 chars to keep the result a valid DNS-1123 label.
		{strings.Repeat("x", 60), "-abc", strings.Repeat("x", 59) + "-abc"},
	}
	for _, tc := range cases {
		got := suffixName(tc.name, tc.suffix)
		if got != tc.want {
			t.Errorf("suffixName(%q,%q) = %q, want %q", tc.name, tc.suffix, got, tc.want)
		}
		if len(got) > 63 {
			t.Errorf("suffixName(%q,%q) = %q exceeds the 63-char label limit", tc.name, tc.suffix, got)
		}
	}
}

func TestBackoff(t *testing.T) {
	t.Parallel()
	if got := backoff(0); got != 500*time.Millisecond {
		t.Errorf("backoff(0) = %v, want 500ms", got)
	}
	if got := backoff(1); got != time.Second {
		t.Errorf("backoff(1) = %v, want 1s", got)
	}
	// Later attempts saturate at the 5s ceiling.
	if got := backoff(20); got != 5*time.Second {
		t.Errorf("backoff(20) = %v, want the 5s ceiling", got)
	}
}

func TestSecretValue(t *testing.T) {
	t.Parallel()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("corev1 AddToScheme: %v", err)
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "gate-token", Namespace: "ns"},
		Data:       map[string][]byte{"token": []byte("s3cr3t")},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(sec).Build()
	e := &Executor{Client: c}

	got, err := e.secretValue(context.Background(), "ns", &meta.SecretKeyReference{Name: "gate-token", Key: "token"})
	if err != nil {
		t.Fatalf("secretValue: %v", err)
	}
	if got != "s3cr3t" {
		t.Errorf("secretValue = %q, want %q", got, "s3cr3t")
	}

	if _, err := e.secretValue(context.Background(), "ns", &meta.SecretKeyReference{Name: "gate-token", Key: "absent"}); err == nil {
		t.Error("a missing key must error")
	}
	if _, err := e.secretValue(context.Background(), "ns", &meta.SecretKeyReference{Name: "absent", Key: "token"}); err == nil {
		t.Error("a missing Secret must error")
	}
}

// dynScheme registers the core types plus an unstructured ConfigMap so a fake
// client serves Get/Patch/Delete on them.
func dynScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("corev1 AddToScheme: %v", err)
	}
	return s
}

func configMap(t *testing.T, ns, name string) *corev1.ConfigMap {
	t.Helper()
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Data:       map[string]string{"k": "v"},
	}
}

// TestRun_DeleteAction covers the documented `delete` action type: a present
// object is removed, and a missing object counts as success (idempotent).
func TestRun_DeleteAction(t *testing.T) {
	t.Parallel()
	cm := configMap(t, "ns", "legacy")
	c := fake.NewClientBuilder().WithScheme(dynScheme(t)).WithObjects(cm).Build()
	e := &Executor{Client: c}

	del := stagesv1.Action{Name: "drop", Delete: &stagesv1.DeleteAction{
		Target: meta.NamespacedObjectKindReference{APIVersion: "v1", Kind: "ConfigMap", Name: "legacy"},
	}}
	if err := e.Run(context.Background(), "ns", []stagesv1.Action{del}, nil, nil); err != nil {
		t.Fatalf("delete present object: %v", err)
	}
	var got corev1.ConfigMap
	if err := c.Get(context.Background(), apitypes.NamespacedName{Namespace: "ns", Name: "legacy"}, &got); !apierrors.IsNotFound(err) {
		t.Fatalf("object should be gone, Get err = %v", err)
	}

	// A second delete of the now-absent object is a no-op success.
	if err := e.Run(context.Background(), "ns", []stagesv1.Action{del}, nil, nil); err != nil {
		t.Fatalf("delete of a missing object must succeed: %v", err)
	}
}

func TestRun_DeleteAction_BadAPIVersion(t *testing.T) {
	t.Parallel()
	e := &Executor{Client: fake.NewClientBuilder().WithScheme(dynScheme(t)).Build()}
	del := stagesv1.Action{Name: "drop", Delete: &stagesv1.DeleteAction{
		Target: meta.NamespacedObjectKindReference{APIVersion: "a/b/c", Kind: "ConfigMap", Name: "x"},
	}}
	if err := e.Run(context.Background(), "ns", []stagesv1.Action{del}, nil, nil); err == nil {
		t.Fatal("a malformed apiVersion must error")
	}
}

// TestRun_PatchAction covers the documented `patch` action type (strategic
// merge), defaulting the target namespace to the run namespace.
func TestRun_PatchAction(t *testing.T) {
	t.Parallel()
	cm := configMap(t, "ns", "web")
	c := fake.NewClientBuilder().WithScheme(dynScheme(t)).WithObjects(cm).Build()
	e := &Executor{Client: c}

	patch := stagesv1.Action{Name: "flip", Patch: &stagesv1.PatchAction{
		Target: meta.NamespacedObjectKindReference{APIVersion: "v1", Kind: "ConfigMap", Name: "web"},
		Type:   "merge",
		Patch:  `{"data":{"k":"patched"}}`,
	}}
	if err := e.Run(context.Background(), "ns", []stagesv1.Action{patch}, nil, nil); err != nil {
		t.Fatalf("patch: %v", err)
	}
	var got corev1.ConfigMap
	if err := c.Get(context.Background(), apitypes.NamespacedName{Namespace: "ns", Name: "web"}, &got); err != nil {
		t.Fatalf("re-get: %v", err)
	}
	if got.Data["k"] != "patched" {
		t.Fatalf("patch not applied: data = %#v", got.Data)
	}
}

func TestRun_PatchAction_BadAPIVersion(t *testing.T) {
	t.Parallel()
	e := &Executor{Client: fake.NewClientBuilder().WithScheme(dynScheme(t)).Build()}
	patch := stagesv1.Action{Name: "flip", Patch: &stagesv1.PatchAction{
		Target: meta.NamespacedObjectKindReference{APIVersion: "a/b/c", Kind: "ConfigMap", Name: "x"},
		Patch:  `{}`,
	}}
	if err := e.Run(context.Background(), "ns", []stagesv1.Action{patch}, nil, nil); err == nil {
		t.Fatal("a malformed apiVersion must error")
	}
}

// TestRun_ApplyAction_FailsClosed pins the documented `apply` action type's
// fail-closed behaviour without the resolver/fetcher/applier wiring.
func TestRun_ApplyAction_FailsClosed(t *testing.T) {
	t.Parallel()
	e := &Executor{Client: fake.NewClientBuilder().WithScheme(dynScheme(t)).Build()}
	apply := stagesv1.Action{Name: "stand-up", Apply: &stagesv1.ApplyAction{
		SourceRef: stagesv1.SourceReference{Name: "maint"},
	}}
	if err := e.Run(context.Background(), "ns", []stagesv1.Action{apply}, nil, nil); !errors.Is(err, ErrActionUnsupported) {
		t.Fatalf("apply without wiring: want ErrActionUnsupported, got %v", err)
	}
}

// TestPollExpr covers the CEL `wait` arm: the expression is checked against the
// target's live state, and a satisfied expression returns immediately.
func TestPollExpr_SatisfiedExpressionReturns(t *testing.T) {
	t.Parallel()
	dep := &unstructured.Unstructured{}
	dep.SetGroupVersionKind(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"})
	dep.SetNamespace("ns")
	dep.SetName("web")
	_ = unstructured.SetNestedField(dep.Object, int64(3), "status", "availableReplicas")

	s := runtime.NewScheme()
	gvk := dep.GroupVersionKind()
	s.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
	list := gvk
	list.Kind += "List"
	s.AddKnownTypeWithName(list, &unstructured.UnstructuredList{})
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(dep).Build()
	e := &Executor{Client: c}

	target := &meta.NamespacedObjectKindReference{APIVersion: "apps/v1", Kind: "Deployment", Name: "web"}
	err := e.pollExpr(context.Background(), "ns", target, "status.availableReplicas >= 3", &metav1.Duration{Duration: 2 * time.Second})
	if err != nil {
		t.Fatalf("pollExpr on a satisfied expr: %v", err)
	}
}

func TestPollExpr_CompileErrorPropagates(t *testing.T) {
	t.Parallel()
	e := &Executor{Client: fake.NewClientBuilder().WithScheme(dynScheme(t)).Build()}
	target := &meta.NamespacedObjectKindReference{APIVersion: "apps/v1", Kind: "Deployment", Name: "web"}
	if err := e.pollExpr(context.Background(), "ns", target, "this is not (valid CEL", nil); err == nil {
		t.Fatal("an uncompilable CEL expression must error before polling")
	}
}

func TestPollExpr_TimesOutWhenNeverSatisfied(t *testing.T) {
	t.Parallel()
	dep := &unstructured.Unstructured{}
	dep.SetGroupVersionKind(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"})
	dep.SetNamespace("ns")
	dep.SetName("web")
	_ = unstructured.SetNestedField(dep.Object, int64(0), "status", "availableReplicas")

	s := runtime.NewScheme()
	gvk := dep.GroupVersionKind()
	s.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
	list := gvk
	list.Kind += "List"
	s.AddKnownTypeWithName(list, &unstructured.UnstructuredList{})
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(dep).Build()
	e := &Executor{Client: c}

	target := &meta.NamespacedObjectKindReference{APIVersion: "apps/v1", Kind: "Deployment", Name: "web"}
	err := e.pollExpr(context.Background(), "ns", target, "status.availableReplicas >= 3", &metav1.Duration{Duration: 50 * time.Millisecond})
	if err == nil {
		t.Fatal("an expression that never holds must time out")
	}
}
