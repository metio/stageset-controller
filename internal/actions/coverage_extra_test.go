// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package actions

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fluxcd/pkg/apis/meta"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	apitypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/artifact"
)

// makeArtifactTarGz builds a deterministic gzip+tar artifact from a path->content
// map for the job/apply Fetcher path.
func makeArtifactTarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header: %v", err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("write body: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return buf.Bytes()
}

func sha256DigestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// permissiveFetcher reaches httptest loopback listeners through the artifact
// Fetcher's URL/IP guard seams.
func permissiveFetcher() *artifact.Fetcher {
	return &artifact.Fetcher{
		HTTPClient:           http.DefaultClient,
		URLValidator:         artifact.PermissiveHTTPURL,
		IPValidator:          artifact.PermissiveIP,
		MaxArchiveBytes:      1 << 20,
		MaxPerEntryBytes:     1 << 20,
		MaxDecompressedBytes: 1 << 20,
		MaxExtractedBytes:    1 << 20,
	}
}

// eaScheme registers the ExternalArtifact GVK (plus its List) as unstructured so
// a fake client serves Get on it, alongside the core types.
func eaScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("corev1 AddToScheme: %v", err)
	}
	gvk := schema.GroupVersionKind{Group: "source.toolkit.fluxcd.io", Version: "v1", Kind: "ExternalArtifact"}
	s.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
	list := gvk
	list.Kind += "List"
	s.AddKnownTypeWithName(list, &unstructured.UnstructuredList{})
	// Jobs created by the job action are served as unstructured too.
	jobGVK := schema.GroupVersionKind{Group: "batch", Version: "v1", Kind: "Job"}
	s.AddKnownTypeWithName(jobGVK, &unstructured.Unstructured{})
	jobList := jobGVK
	jobList.Kind += "List"
	s.AddKnownTypeWithName(jobList, &unstructured.UnstructuredList{})
	return s
}

// readyExternalArtifact builds an ExternalArtifact whose status carries a Ready
// condition and the artifact url/revision/digest pointing at srvURL.
func readyExternalArtifact(t *testing.T, ns, name, srvURL, digest string) *unstructured.Unstructured {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: "source.toolkit.fluxcd.io", Version: "v1", Kind: "ExternalArtifact"})
	u.SetNamespace(ns)
	u.SetName(name)
	if err := unstructured.SetNestedSlice(u.Object, []any{map[string]any{
		"type":   "Ready",
		"status": "True",
		"reason": "Succeeded",
	}}, "status", "conditions"); err != nil {
		t.Fatalf("set conditions: %v", err)
	}
	if err := unstructured.SetNestedMap(u.Object, map[string]any{
		"url":      srvURL,
		"revision": "rev-1",
		"digest":   digest,
	}, "status", "artifact"); err != nil {
		t.Fatalf("set artifact: %v", err)
	}
	return u
}

// stubApplier records the objects and arguments it was asked to apply, and can be
// configured to fail.
type stubApplier struct {
	called  int32
	gotWait bool
	gotN    int
	err     error
}

func (s *stubApplier) Apply(_ context.Context, objects []*unstructured.Unstructured, wait bool, _ time.Duration) error {
	atomic.AddInt32(&s.called, 1)
	s.gotWait = wait
	s.gotN = len(objects)
	return s.err
}

// TestApplyManifests_HappyPath exercises the full apply path: resolve an
// ExternalArtifact, fetch+build its manifests, and hand them to the Applier with
// the action's wait flag.
func TestApplyManifests_HappyPath(t *testing.T) {
	t.Parallel()
	tarball := makeArtifactTarGz(t, map[string]string{
		"manifests/cm.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: maint\n  namespace: ns\ndata:\n  on: \"true\"\n",
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(tarball)
	}))
	defer srv.Close()

	ea := readyExternalArtifact(t, "ns", "maint-src", srv.URL, sha256DigestOf(tarball))
	c := fake.NewClientBuilder().WithScheme(eaScheme(t)).WithObjects(ea).Build()
	ap := &stubApplier{}
	e := &Executor{
		Client:   c,
		Resolver: &artifact.Resolver{},
		Fetcher:  permissiveFetcher(),
		Applier:  ap,
	}

	act := stagesv1.Action{Name: "maint-up", Apply: &stagesv1.ApplyAction{
		SourceRef: stagesv1.SourceReference{Name: "maint-src"},
		Path:      "manifests",
		Wait:      true,
	}}
	if err := e.Run(context.Background(), "ns", []stagesv1.Action{act}, nil, nil); err != nil {
		t.Fatalf("applyManifests happy path: %v", err)
	}
	if atomic.LoadInt32(&ap.called) != 1 {
		t.Fatalf("Applier should be called once, called=%d", ap.called)
	}
	if !ap.gotWait {
		t.Fatal("the action's wait flag must reach the Applier")
	}
	if ap.gotN != 1 {
		t.Fatalf("expected 1 built object, got %d", ap.gotN)
	}
}

// TestApplyManifests_ResolveError covers the resolve-failure branch: a missing
// ExternalArtifact surfaces as a wrapped resolve error.
func TestApplyManifests_ResolveError(t *testing.T) {
	t.Parallel()
	c := fake.NewClientBuilder().WithScheme(eaScheme(t)).Build()
	e := &Executor{
		Client:   c,
		Resolver: &artifact.Resolver{},
		Fetcher:  permissiveFetcher(),
		Applier:  &stubApplier{},
	}
	act := stagesv1.Action{Name: "maint-up", Apply: &stagesv1.ApplyAction{
		SourceRef: stagesv1.SourceReference{Name: "absent"},
	}}
	err := e.Run(context.Background(), "ns", []stagesv1.Action{act}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "resolve apply sourceRef") {
		t.Fatalf("missing apply source should fail with a resolve error, got %v", err)
	}
}

// TestApplyManifests_ApplierError covers the Applier returning an error after a
// successful resolve/fetch/build.
func TestApplyManifests_ApplierError(t *testing.T) {
	t.Parallel()
	tarball := makeArtifactTarGz(t, map[string]string{
		"cm.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: maint\n  namespace: ns\n",
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(tarball)
	}))
	defer srv.Close()

	ea := readyExternalArtifact(t, "ns", "maint-src", srv.URL, sha256DigestOf(tarball))
	c := fake.NewClientBuilder().WithScheme(eaScheme(t)).WithObjects(ea).Build()
	e := &Executor{
		Client:   c,
		Resolver: &artifact.Resolver{},
		Fetcher:  permissiveFetcher(),
		Applier:  &stubApplier{err: errors.New("apply boom")},
	}
	act := stagesv1.Action{Name: "maint-up", Apply: &stagesv1.ApplyAction{
		SourceRef: stagesv1.SourceReference{Name: "maint-src"},
	}}
	err := e.Run(context.Background(), "ns", []stagesv1.Action{act}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "apply boom") {
		t.Fatalf("an Applier failure must propagate, got %v", err)
	}
}

// TestJob_HappyPath exercises the full job path: resolve, fetch+build a Job, create
// it with a revision-derived suffix, await its Complete condition, then garbage
// collect it (cleanupObjects).
func TestJob_HappyPath(t *testing.T) {
	t.Parallel()
	tarball := makeArtifactTarGz(t, map[string]string{
		"migrate.yaml": "apiVersion: batch/v1\nkind: Job\nmetadata:\n  name: migrate\n  namespace: ns\nspec:\n  template:\n    spec:\n      containers:\n      - name: m\n        image: busybox\n      restartPolicy: Never\n",
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(tarball)
	}))
	defer srv.Close()

	ea := readyExternalArtifact(t, "ns", "migrate-src", srv.URL, sha256DigestOf(tarball))

	// Pre-seed the Job that the action will create, already Complete, so awaitJob
	// returns on its first poll. The action's suffix is derived from the revision
	// ("rev-1"); compute the exact created name so the seeded object matches.
	suffix := "-" + shortHash("rev-1")
	jobName := suffixName("migrate", suffix)
	completedJob := jobUnstructured("ns", jobName, "Complete", "True")

	c := fake.NewClientBuilder().WithScheme(eaScheme(t)).WithObjects(ea, completedJob).Build()
	e := &Executor{
		Client:   c,
		Resolver: &artifact.Resolver{},
		Fetcher:  permissiveFetcher(),
	}
	act := stagesv1.Action{Name: "db-migrate", Job: &stagesv1.JobAction{
		SourceRef: stagesv1.SourceReference{Name: "migrate-src"},
	}}
	if err := e.Run(context.Background(), "ns", []stagesv1.Action{act}, nil, nil); err != nil {
		t.Fatalf("job happy path: %v", err)
	}
	// cleanupObjects (deferred) deletes the created Job after completion.
	var got unstructured.Unstructured
	got.SetGroupVersionKind(schema.GroupVersionKind{Group: "batch", Version: "v1", Kind: "Job"})
	gerr := c.Get(context.Background(), apitypes.NamespacedName{Namespace: "ns", Name: jobName}, &got)
	if gerr == nil {
		t.Fatal("the job should be garbage-collected after completion")
	}
}

// TestJob_ResolveError covers the resolve-failure branch of the job action.
func TestJob_ResolveError(t *testing.T) {
	t.Parallel()
	c := fake.NewClientBuilder().WithScheme(eaScheme(t)).Build()
	e := &Executor{
		Client:   c,
		Resolver: &artifact.Resolver{},
		Fetcher:  permissiveFetcher(),
	}
	act := stagesv1.Action{Name: "db-migrate", Job: &stagesv1.JobAction{
		SourceRef: stagesv1.SourceReference{Name: "absent"},
	}}
	err := e.Run(context.Background(), "ns", []stagesv1.Action{act}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "resolve job artifact") {
		t.Fatalf("missing job source should fail with a resolve error, got %v", err)
	}
}

// TestJob_BuildError covers the build-failure branch: a Path pointing at a
// directory absent from the artifact fails the build step.
func TestJob_BuildError(t *testing.T) {
	t.Parallel()
	tarball := makeArtifactTarGz(t, map[string]string{"j.yaml": "apiVersion: batch/v1\nkind: Job\nmetadata:\n  name: j\n"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(tarball)
	}))
	defer srv.Close()

	ea := readyExternalArtifact(t, "ns", "bad-src", srv.URL, sha256DigestOf(tarball))
	c := fake.NewClientBuilder().WithScheme(eaScheme(t)).WithObjects(ea).Build()
	e := &Executor{
		Client:   c,
		Resolver: &artifact.Resolver{},
		Fetcher:  permissiveFetcher(),
	}
	act := stagesv1.Action{Name: "db-migrate", Job: &stagesv1.JobAction{
		SourceRef: stagesv1.SourceReference{Name: "bad-src"},
		Path:      "no-such-dir",
	}}
	if err := e.Run(context.Background(), "ns", []stagesv1.Action{act}, nil, nil); err == nil {
		t.Fatal("a job whose Path is absent from the artifact must fail the build")
	}
}

// TestJob_FetchError covers the fetch-failure branch: the artifact URL serves a
// digest that does not match status.artifact.digest.
func TestJob_FetchError(t *testing.T) {
	t.Parallel()
	tarball := makeArtifactTarGz(t, map[string]string{"j.yaml": "apiVersion: batch/v1\nkind: Job\nmetadata:\n  name: j\n"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(tarball)
	}))
	defer srv.Close()

	// Claim a digest the served body does not have so verifyDigest fails.
	ea := readyExternalArtifact(t, "ns", "mismatch-src", srv.URL, "sha256:"+strings.Repeat("0", 64))
	c := fake.NewClientBuilder().WithScheme(eaScheme(t)).WithObjects(ea).Build()
	e := &Executor{
		Client:   c,
		Resolver: &artifact.Resolver{},
		Fetcher:  permissiveFetcher(),
	}
	act := stagesv1.Action{Name: "db-migrate", Job: &stagesv1.JobAction{
		SourceRef: stagesv1.SourceReference{Name: "mismatch-src"},
	}}
	err := e.Run(context.Background(), "ns", []stagesv1.Action{act}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "fetch job artifact") {
		t.Fatalf("a digest mismatch must fail the fetch step, got %v", err)
	}
}

// TestCleanupObjects deletes a set of objects under a client, tolerating an
// already-absent one (idempotent best-effort GC).
func TestCleanupObjects(t *testing.T) {
	t.Parallel()
	present := jobUnstructured("ns", "present", "Complete", "True")
	absent := jobUnstructured("ns", "absent", "Complete", "True")
	c := jobClient(t, present) // only "present" exists
	cleanupObjects(c, []*unstructured.Unstructured{present, absent})

	var got unstructured.Unstructured
	got.SetGroupVersionKind(present.GroupVersionKind())
	if err := c.Get(context.Background(), apitypes.NamespacedName{Namespace: "ns", Name: "present"}, &got); err == nil {
		t.Fatal("cleanupObjects should have deleted the present object")
	}
}

// TestWait_NoOpWhenEmpty covers the wait arm where neither Duration nor Expr is
// set: it returns nil immediately without touching the cluster.
func TestWait_NoOpWhenEmpty(t *testing.T) {
	t.Parallel()
	e := &Executor{}
	err := e.Run(context.Background(), "ns",
		[]stagesv1.Action{{Name: "noop", Wait: &stagesv1.WaitAction{}}}, nil, nil)
	if err != nil {
		t.Fatalf("an empty wait must be a no-op success, got %v", err)
	}
}

// TestWait_DurationContextCancelled covers the ctx.Done() arm of a fixed-duration
// wait: a cancelled context returns its error before the duration elapses.
func TestWait_DurationContextCancelled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e := &Executor{}
	err := e.wait(ctx, "ns", &stagesv1.WaitAction{Duration: &metav1.Duration{Duration: time.Hour}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled context during a duration wait must return its error, got %v", err)
	}
}

// TestExec_RetriesThenSucceeds proves the retry/backoff loop: a transient 5xx on
// the first attempt, then a 200, succeeds within the retry budget.
func TestExec_RetriesThenSucceeds(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	e := &Executor{HTTPClient: http.DefaultClient, AllowedHosts: []string{hostOf(t, srv.URL)}}
	err := e.Run(context.Background(), "ns",
		[]stagesv1.Action{{Name: "ping", HTTP: &stagesv1.HTTPAction{URL: srv.URL}, Retries: ptrInt32(3)}}, nil, nil)
	if err != nil {
		t.Fatalf("a transient failure that recovers must succeed within the budget: %v", err)
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("expected one failure then one success, hits=%d", got)
	}
}

// TestExec_TimeoutAppliesPerAction proves the per-action timeout wraps the
// dispatch context: a slow server combined with a tiny action timeout fails.
func TestExec_TimeoutAppliesPerAction(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
			w.WriteHeader(http.StatusOK)
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()

	e := &Executor{HTTPClient: http.DefaultClient, AllowedHosts: []string{hostOf(t, srv.URL)}}
	err := e.Run(context.Background(), "ns", []stagesv1.Action{{
		Name:    "slow",
		HTTP:    &stagesv1.HTTPAction{URL: srv.URL},
		Timeout: &metav1.Duration{Duration: 50 * time.Millisecond},
	}}, nil, nil)
	if err == nil {
		t.Fatal("a slow request under a tiny per-action timeout must fail")
	}
}

// TestExec_RecordError proves a failing record callback aborts the run after the
// action's side effect already fired.
func TestExec_RecordError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	e := &Executor{HTTPClient: http.DefaultClient, AllowedHosts: []string{hostOf(t, srv.URL)}}
	recErr := errors.New("ledger write failed")
	err := e.Run(context.Background(), "ns",
		[]stagesv1.Action{{Name: "ping", HTTP: &stagesv1.HTTPAction{URL: srv.URL}}},
		nil, func(string) error { return recErr })
	if !errors.Is(err, recErr) {
		t.Fatalf("a record failure must abort the run, got %v", err)
	}
}

// TestDispatch_NoVerb proves an action that sets no verb is rejected.
func TestDispatch_NoVerb(t *testing.T) {
	t.Parallel()
	e := &Executor{}
	err := e.Run(context.Background(), "ns",
		[]stagesv1.Action{{Name: "empty"}}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "sets no verb") {
		t.Fatalf("an action with no verb must error, got %v", err)
	}
}

// TestHTTPAction_BodyFromSecret proves the request body is sourced from a Secret
// via BodyFrom and delivered to the server.
func TestHTTPAction_BodyFromSecret(t *testing.T) {
	t.Parallel()
	var gotBody atomic.Value
	gotBody.Store("")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		gotBody.Store(string(b))
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("corev1 AddToScheme: %v", err)
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "payload", Namespace: "ns"},
		Data:       map[string][]byte{"body": []byte("hello-from-secret")},
	}
	e := &Executor{
		Client:       fake.NewClientBuilder().WithScheme(s).WithObjects(sec).Build(),
		HTTPClient:   http.DefaultClient,
		AllowedHosts: []string{hostOf(t, srv.URL)},
	}
	act := stagesv1.Action{Name: "notify", HTTP: &stagesv1.HTTPAction{
		URL:      srv.URL,
		BodyFrom: &meta.SecretKeyReference{Name: "payload", Key: "body"},
	}}
	if err := e.Run(context.Background(), "ns", []stagesv1.Action{act}, nil, nil); err != nil {
		t.Fatalf("BodyFrom request: %v", err)
	}
	if got := gotBody.Load().(string); got != "hello-from-secret" {
		t.Fatalf("server received body %q, want the secret value", got)
	}
}

// TestHTTPAction_BodyFromSecretMissing proves a BodyFrom reference to an absent
// Secret fails the action before any request is made.
func TestHTTPAction_BodyFromSecretMissing(t *testing.T) {
	t.Parallel()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("corev1 AddToScheme: %v", err)
	}
	e := &Executor{
		Client:       fake.NewClientBuilder().WithScheme(s).Build(),
		HTTPClient:   http.DefaultClient,
		AllowedHosts: []string{hostOf(t, srv.URL)},
	}
	act := stagesv1.Action{Name: "notify", HTTP: &stagesv1.HTTPAction{
		URL:      srv.URL,
		BodyFrom: &meta.SecretKeyReference{Name: "absent", Key: "body"},
	}}
	if err := e.Run(context.Background(), "ns", []stagesv1.Action{act}, nil, nil); err == nil {
		t.Fatal("a missing BodyFrom secret must fail the action")
	}
	if atomic.LoadInt32(&hits) != 0 {
		t.Fatalf("the request must not fire when BodyFrom resolution fails, hits=%d", hits)
	}
}

// TestHTTPAction_HeaderFromSecretMissing proves a HeadersFrom reference to an
// absent Secret fails before the request.
func TestHTTPAction_HeaderFromSecretMissing(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("corev1 AddToScheme: %v", err)
	}
	e := &Executor{
		Client:       fake.NewClientBuilder().WithScheme(s).Build(),
		HTTPClient:   http.DefaultClient,
		AllowedHosts: []string{hostOf(t, srv.URL)},
	}
	act := stagesv1.Action{Name: "notify", HTTP: &stagesv1.HTTPAction{
		URL:         srv.URL,
		HeadersFrom: []meta.SecretKeyReference{{Name: "absent", Key: "X-Api-Key"}},
	}}
	if err := e.Run(context.Background(), "ns", []stagesv1.Action{act}, nil, nil); err == nil {
		t.Fatal("a missing HeadersFrom secret must fail the action")
	}
}

// TestHTTPAction_SameHostRedirectFollowsWithSecret proves the production httpClient
// follows a same-host redirect even when the action carries a secret header, and
// the header reaches the (same-host) target. This drives the production
// httpClient + safeDialContext + resolve path with the dial-time IP guard opted
// in via IPValidator.
func TestHTTPAction_SameHostRedirectFollowsWithSecret(t *testing.T) {
	t.Parallel()
	var sawKey int32
	mux := http.NewServeMux()
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/done", http.StatusFound)
	})
	mux.HandleFunc("/done", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") == "SECRET" {
			atomic.AddInt32(&sawKey, 1)
		}
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("corev1 AddToScheme: %v", err)
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: "ns"},
		Data:       map[string][]byte{"X-Api-Key": []byte("SECRET")},
	}
	srvHost := hostOf(t, srv.URL)
	e := &Executor{
		Client:       fake.NewClientBuilder().WithScheme(s).WithObjects(sec).Build(),
		AllowedHosts: []string{srvHost},
		IPValidator:  PermissiveIP,
		lookupIP: func(_ context.Context, host string) ([]net.IP, error) {
			return net.DefaultResolver.LookupIP(context.Background(), "ip", host)
		},
	}
	act := stagesv1.Action{Name: "notify", HTTP: &stagesv1.HTTPAction{
		URL:         srv.URL + "/start",
		HeadersFrom: []meta.SecretKeyReference{{Name: "creds", Key: "X-Api-Key"}},
	}}
	if err := e.Run(context.Background(), "ns", []stagesv1.Action{act}, nil, nil); err != nil {
		t.Fatalf("same-host redirect carrying a secret must be followed: %v", err)
	}
	if atomic.LoadInt32(&sawKey) != 1 {
		t.Fatalf("the secret header must reach the same-host redirect target, sawKey=%d", sawKey)
	}
}

// TestHTTPAction_ExpectedStatusCustom proves a non-2xx code listed in
// expectedStatus is accepted (the explicit-acceptance branch).
func TestHTTPAction_ExpectedStatusCustom(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	defer srv.Close()

	e := &Executor{HTTPClient: http.DefaultClient, AllowedHosts: []string{hostOf(t, srv.URL)}}
	act := stagesv1.Action{Name: "ping", HTTP: &stagesv1.HTTPAction{
		URL:            srv.URL,
		ExpectedStatus: []int32{418},
	}}
	if err := e.Run(context.Background(), "ns", []stagesv1.Action{act}, nil, nil); err != nil {
		t.Fatalf("a 418 listed in expectedStatus must be accepted: %v", err)
	}
}

// TestPatch_JSON6902TypeMapping proves spec.type "json6902" maps to a JSON-Patch
// (a successful op against a fake-client object).
func TestPatch_JSON6902TypeMapping(t *testing.T) {
	t.Parallel()
	cm := configMap(t, "ns", "web")
	c := fake.NewClientBuilder().WithScheme(dynScheme(t)).WithObjects(cm).Build()
	e := &Executor{Client: c}

	act := stagesv1.Action{Name: "flip", Patch: &stagesv1.PatchAction{
		Target: stagesv1.PatchTarget{APIVersion: "v1", Kind: "ConfigMap", Name: "web"},
		Type:   "json6902",
		Patch:  `[{"op":"replace","path":"/data/k","value":"patched"}]`,
	}}
	if err := e.Run(context.Background(), "ns", []stagesv1.Action{act}, nil, nil); err != nil {
		t.Fatalf("json6902 patch: %v", err)
	}
	var got corev1.ConfigMap
	if err := c.Get(context.Background(), apitypes.NamespacedName{Namespace: "ns", Name: "web"}, &got); err != nil {
		t.Fatalf("re-get: %v", err)
	}
	if got.Data["k"] != "patched" {
		t.Fatalf("json6902 patch not applied: data = %#v", got.Data)
	}
}

// TestSafeDialContext_BadAddr covers the SplitHostPort failure branch.
func TestSafeDialContext_BadAddr(t *testing.T) {
	t.Parallel()
	e := &Executor{}
	if _, err := e.safeDialContext(context.Background(), "tcp", "no-port-here"); err == nil {
		t.Fatal("a malformed addr must fail SplitHostPort")
	}
}

// TestSafeDialContext_ResolveError covers the resolve-failure branch via the
// lookupIP seam.
func TestSafeDialContext_ResolveError(t *testing.T) {
	t.Parallel()
	e := &Executor{
		lookupIP: func(_ context.Context, _ string) ([]net.IP, error) {
			return nil, errors.New("dns boom")
		},
	}
	if _, err := e.safeDialContext(context.Background(), "tcp", "example.com:443"); err == nil {
		t.Fatal("a resolve failure must surface from safeDialContext")
	}
}

// TestSafeDialContext_NoAddresses covers the empty-resolution branch: a host that
// resolves to no addresses yields the "no addresses" error.
func TestSafeDialContext_NoAddresses(t *testing.T) {
	t.Parallel()
	e := &Executor{
		IPValidator: PermissiveIP,
		lookupIP: func(_ context.Context, _ string) ([]net.IP, error) {
			return []net.IP{}, nil
		},
	}
	_, err := e.safeDialContext(context.Background(), "tcp", "example.com:443")
	if err == nil || !strings.Contains(err.Error(), "no addresses") {
		t.Fatalf("an empty resolution must report no addresses, got %v", err)
	}
}

// TestResolve_DefaultResolverError exercises the production resolve path (no
// lookupIP seam) against a name that does not resolve.
func TestResolve_DefaultResolverError(t *testing.T) {
	t.Parallel()
	e := &Executor{}
	_, err := e.resolve(context.Background(), "this-host-does-not-exist.invalid")
	if err == nil {
		t.Fatal("an unresolvable host must error through the default resolver")
	}
}

// TestResolve_DefaultResolverSuccess exercises the production resolve happy path
// (net.DefaultResolver) against loopback, which always resolves.
func TestResolve_DefaultResolverSuccess(t *testing.T) {
	t.Parallel()
	e := &Executor{}
	ips, err := e.resolve(context.Background(), "localhost")
	if err != nil {
		t.Fatalf("localhost must resolve through the default resolver: %v", err)
	}
	if len(ips) == 0 {
		t.Fatal("localhost should resolve to at least one address")
	}
}

// TestForbiddenIP covers the production IP denylist directly.
func TestForbiddenIP(t *testing.T) {
	t.Parallel()
	cases := []struct {
		ip   string
		want bool
	}{
		{"127.0.0.1", true},
		{"169.254.169.254", true},
		{"224.0.0.1", true},
		{"0.0.0.0", true},
		{"8.8.8.8", false},
		{"10.0.0.1", false},
	}
	for _, tc := range cases {
		if got := forbiddenIP(net.ParseIP(tc.ip)); got != tc.want {
			t.Errorf("forbiddenIP(%s) = %v, want %v", tc.ip, got, tc.want)
		}
	}
}

// TestIPValidator_ProductionRejectsForbidden proves the default ipValidator (no
// IPValidator seam) rejects a forbidden address with ErrForbiddenAddress.
func TestIPValidator_ProductionRejectsForbidden(t *testing.T) {
	t.Parallel()
	e := &Executor{}
	check := e.ipValidator()
	if err := check(net.ParseIP("127.0.0.1")); !errors.Is(err, ErrForbiddenAddress) {
		t.Fatalf("default ipValidator must reject loopback, got %v", err)
	}
	if err := check(net.ParseIP("8.8.8.8")); err != nil {
		t.Fatalf("default ipValidator must allow a public address, got %v", err)
	}
}

// TestStatusClass covers the classifier across the terminal-vs-retryable boundary.
func TestStatusClass(t *testing.T) {
	t.Parallel()
	if !errors.Is(statusClass(http.StatusBadRequest), ErrHTTPClientStatus) {
		t.Fatal("400 must be terminal client-error")
	}
	if !errors.Is(statusClass(http.StatusUnauthorized), ErrHTTPClientStatus) {
		t.Fatal("401 must be terminal client-error")
	}
	if errors.Is(statusClass(http.StatusTooManyRequests), ErrHTTPClientStatus) {
		t.Fatal("429 must not be terminal (it is retryable)")
	}
	if errors.Is(statusClass(http.StatusRequestTimeout), ErrHTTPClientStatus) {
		t.Fatal("408 must not be terminal (it is retryable)")
	}
	if errors.Is(statusClass(http.StatusInternalServerError), ErrHTTPClientStatus) {
		t.Fatal("500 must not be terminal client-error")
	}
}

// TestBodySnippet covers the empty-body label and trimming.
func TestBodySnippet(t *testing.T) {
	t.Parallel()
	if got := bodySnippet([]byte("  \n\t ")); got != "(empty)" {
		t.Fatalf("whitespace-only body should render as (empty), got %q", got)
	}
	if got := bodySnippet([]byte("  oops  ")); got != "oops" {
		t.Fatalf("a body should be trimmed, got %q", got)
	}
}

// TestFoldLastErr covers both branches of the timeout-folding helper.
func TestFoldLastErr(t *testing.T) {
	t.Parallel()
	to := context.DeadlineExceeded
	if got := foldLastErr(to, nil); !errors.Is(got, to) {
		t.Fatalf("with no last error, the timeout must stand alone, got %v", got)
	}
	last := errors.New("denied get")
	folded := foldLastErr(to, last)
	if !errors.Is(folded, to) || !strings.Contains(folded.Error(), "denied get") {
		t.Fatalf("the folded error must carry both the timeout and the last error, got %v", folded)
	}
}

// TestAwaitJob_ContextCancelled proves awaitJob folds a Get failure into the
// timeout message when the context is already cancelled and the job is absent.
func TestAwaitJob_ContextCancelled(t *testing.T) {
	t.Parallel()
	job := jobUnstructured("ns", "gone", "Complete", "True")
	c := jobClient(t) // no objects → Get returns NotFound
	e := &Executor{Client: c}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := e.awaitJob(ctx, job)
	if err == nil || !strings.Contains(err.Error(), "did not complete") {
		t.Fatalf("a cancelled await of an absent job must report it did not complete, got %v", err)
	}
}
