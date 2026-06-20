// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/artifact"
)

func newNamespace(t *testing.T, c client.Client) string {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "stageset-test-"}}
	if err := c.Create(context.Background(), ns); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	t.Cleanup(func() { _ = c.Delete(context.Background(), ns) })
	return ns.Name
}

// plantExternalArtifact creates an ExternalArtifact with a JsonnetSnippet
// back-pointer and, when ready, a Ready=True condition + status.artifact.
func plantExternalArtifact(t *testing.T, c client.Client, ns, name, snippet, revision, digest string, ready bool) {
	t.Helper()
	ctx := context.Background()
	ea := &unstructured.Unstructured{}
	ea.SetGroupVersionKind(externalArtifactGVK)
	ea.SetNamespace(ns)
	ea.SetName(name)
	_ = unstructured.SetNestedMap(ea.Object, map[string]any{
		"apiVersion": "jaas.metio.wtf/v1",
		"kind":       "JsonnetSnippet",
		"name":       snippet,
	}, "spec", "sourceRef")
	if err := c.Create(ctx, ea); err != nil {
		t.Fatalf("create ExternalArtifact: %v", err)
	}

	if !ready {
		return
	}
	_ = unstructured.SetNestedMap(ea.Object, map[string]any{
		"url":      "http://source.flux-system.svc/" + ns + "/" + name + "/x.tar.gz",
		"revision": revision,
		"digest":   digest,
	}, "status", "artifact")
	_ = unstructured.SetNestedSlice(ea.Object, []any{map[string]any{
		"type":   "Ready",
		"status": "True",
		"reason": "Succeeded",
	}}, "status", "conditions")
	if err := c.Status().Update(ctx, ea); err != nil {
		t.Fatalf("update ExternalArtifact status: %v", err)
	}
}

func newStageSet(t *testing.T, c client.Client, ns, name string, ref stagesv1.SourceReference) *stagesv1.StageSet {
	t.Helper()
	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: stagesv1.StageSetSpec{
			Interval: metav1.Duration{Duration: 5 * time.Minute},
			Stages:   []stagesv1.Stage{{Name: "stage-a", SourceRef: ref}},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	return ss
}

func reconcileOnce(t *testing.T, c client.Client, ss *stagesv1.StageSet) {
	t.Helper()
	r := &StageSetReconciler{
		Client:     c,
		RESTMapper: c.RESTMapper(),
		// Permissive fetcher so the artifact's httptest loopback URL is reachable.
		Fetcher: &artifact.Fetcher{HTTPClient: http.DefaultClient, URLValidator: artifact.PermissiveHTTPURL, IPValidator: artifact.PermissiveIP},
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ss.Namespace, Name: ss.Name}}
	if _, err := driveReconcile(r, req); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
}

// driveReconcile runs a reconcile and, if the first pass only added the
// finalizer (which requeues without bumping generation, so the watch predicate
// would otherwise drop the resulting event), runs a second pass so the real
// reconcile happens. It returns the result and error of the run that did the
// work, mirroring how the controller behaves once the finalizer is present.
//
// The finalizer pass is detected by the object having no Ready condition yet:
// the finalizer-add path returns before any status write, so a missing Ready
// means the real reconcile has not run.
func driveReconcile(r *StageSetReconciler, req ctrl.Request) (ctrl.Result, error) {
	res, err := r.Reconcile(context.Background(), req)
	if err != nil {
		return res, err
	}
	var ss stagesv1.StageSet
	if gerr := r.Client.Get(context.Background(), req.NamespacedName, &ss); gerr != nil {
		return res, nil // object gone (e.g. deletion path); nothing more to drive.
	}
	if apimeta.FindStatusCondition(ss.Status.Conditions, ConditionReady) != nil {
		return res, nil
	}
	return r.Reconcile(context.Background(), req)
}

// The first reconcile of a fresh StageSet adds the finalizer and requeues
// without setting Ready: the finalizer add doesn't bump generation, so the
// watch's GenerationChangedPredicate would drop the resulting Update event, and
// the explicit requeue is what re-triggers the real reconcile.
func TestReconcile_FinalizerAddRequeues(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "fresh"},
		Spec: stagesv1.StageSetSpec{
			Interval: metav1.Duration{Duration: 5 * time.Minute},
			Stages:   []stagesv1.Stage{{Name: "stage-a", SourceRef: stagesv1.SourceReference{Name: "missing"}}},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}

	r := &StageSetReconciler{
		Client:     c,
		RESTMapper: c.RESTMapper(),
		Fetcher:    &artifact.Fetcher{HTTPClient: http.DefaultClient, URLValidator: artifact.PermissiveHTTPURL, IPValidator: artifact.PermissiveIP},
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "fresh"}}

	res, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if res.IsZero() {
		t.Fatal("first reconcile should requeue after adding the finalizer, got a zero result")
	}
	got := getStageSet(t, c, ns, "fresh")
	if !controllerContainsFinalizer(got) {
		t.Fatal("finalizer should be present after the first reconcile")
	}
	if r := readyReason(got); r != "" {
		t.Fatalf("Ready should not be set on the finalizer-only pass, got %q", r)
	}

	// The requeued pass runs the real reconcile and reports the missing source.
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if r := readyReason(getStageSet(t, c, ns, "fresh")); r != ReasonArtifactNotFound {
		t.Fatalf("Ready reason = %q, want %q", r, ReasonArtifactNotFound)
	}
}

func controllerContainsFinalizer(ss *stagesv1.StageSet) bool {
	for _, f := range ss.GetFinalizers() {
		if f == FinalizerName {
			return true
		}
	}
	return false
}

func makeTarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatalf("tar header: %v", err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("tar write: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// servedArtifact serves a tarball of files over httptest and plants a ready
// ExternalArtifact pointing at it (with a JsonnetSnippet back-pointer when
// snippet != ""). It returns the pinned revision.
func servedArtifact(t *testing.T, c client.Client, ns, eaName, snippet string, files map[string]string) string {
	t.Helper()
	tarball := makeTarGz(t, files)
	sum := sha256.Sum256(tarball)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(tarball)
	}))
	t.Cleanup(srv.Close)
	revision := "rev1@" + digest

	ctx := context.Background()
	ea := &unstructured.Unstructured{}
	ea.SetGroupVersionKind(externalArtifactGVK)
	ea.SetNamespace(ns)
	ea.SetName(eaName)
	if snippet != "" {
		_ = unstructured.SetNestedMap(ea.Object, map[string]any{
			"apiVersion": "jaas.metio.wtf/v1", "kind": "JsonnetSnippet", "name": snippet,
		}, "spec", "sourceRef")
	}
	if err := c.Create(ctx, ea); err != nil {
		t.Fatalf("create ExternalArtifact: %v", err)
	}
	_ = unstructured.SetNestedMap(ea.Object, map[string]any{
		"url": srv.URL, "revision": revision, "digest": digest,
	}, "status", "artifact")
	_ = unstructured.SetNestedSlice(ea.Object, []any{map[string]any{
		"type": "Ready", "status": "True", "reason": "Succeeded",
	}}, "status", "conditions")
	if err := c.Status().Update(ctx, ea); err != nil {
		t.Fatalf("update ExternalArtifact status: %v", err)
	}
	return revision
}

func configMapManifest(ns, name string) string {
	return "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: " + name + "\n  namespace: " + ns + "\ndata:\n  key: value\n"
}

func getStageSet(t *testing.T, c client.Client, ns, name string) *stagesv1.StageSet {
	t.Helper()
	var ss stagesv1.StageSet
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &ss); err != nil {
		t.Fatalf("get StageSet: %v", err)
	}
	return &ss
}

func readyReason(ss *stagesv1.StageSet) string {
	c := apimeta.FindStatusCondition(ss.Status.Conditions, ConditionReady)
	if c == nil {
		return ""
	}
	return c.Reason
}

// First-adoption baselining emits a MigrationsBaselined event exactly once, so an
// operator can tell "recorded the version, ran nothing" from a real no-op.
func TestReconcile_Migration_BaselineEmitsEvent(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	servedArtifact(t, c, ns, "ea", "", map[string]string{"cm.yaml": configMapManifest(ns, "stage-obj")})
	ss := versionedStageSet(ns, "baseline-ev", "ea", "2.0.0", nil)
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create: %v", err)
	}

	rec := &capturingRecorder{}
	r := &StageSetReconciler{
		Client:     c,
		RESTMapper: c.RESTMapper(),
		Recorder:   rec,
		Fetcher:    &artifact.Fetcher{HTTPClient: http.DefaultClient, URLValidator: artifact.PermissiveHTTPURL, IPValidator: artifact.PermissiveIP},
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "baseline-ev"}}
	if _, err := driveReconcile(r, req); err != nil {
		t.Fatalf("baseline reconcile: %v", err)
	}
	if !rec.has(eventReasonBaselined) {
		t.Fatal("first-adoption baseline should emit a MigrationsBaselined event")
	}
	if getStageSet(t, c, ns, "baseline-ev").Status.Version != "2.0.0" {
		t.Fatal("baseline should record the version")
	}

	// A steady reconcile (no version transition) must not re-baseline / re-emit.
	rec.mu.Lock()
	rec.events = nil
	rec.mu.Unlock()
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("steady reconcile: %v", err)
	}
	if rec.has(eventReasonBaselined) {
		t.Fatal("a steady reconcile must not re-emit the baseline event")
	}
}

// The primary JaaS integration end-to-end: a stage referencing a JsonnetSnippet
// resolves through the ExternalArtifact's RFC-0012 back-pointer, the run pins
// and fetches that artifact, and the rendered manifests are applied to the
// cluster.
func TestReconcile_AppliesProducerArtifact(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)

	rev := servedArtifact(t, c, ns, "dashboards-artifact", "dashboards",
		map[string]string{"cm.yaml": configMapManifest(ns, "deployed")})

	ss := newStageSet(t, c, ns, "platform", stagesv1.SourceReference{
		APIVersion: "jaas.metio.wtf/v1",
		Kind:       "JsonnetSnippet",
		Name:       "dashboards",
	})
	reconcileOnce(t, c, ss)

	// The ConfigMap from the artifact was applied, carrying the owner label.
	var deployed corev1.ConfigMap
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "deployed"}, &deployed); err != nil {
		t.Fatalf("artifact ConfigMap was not applied: %v", err)
	}
	if deployed.Labels["stages.metio.wtf/name"] != "platform" {
		t.Fatalf("owner label not stamped: %#v", deployed.Labels)
	}

	got := getStageSet(t, c, ns, "platform")
	if r := readyReason(got); r != ReasonReady {
		t.Fatalf("Ready reason = %q, want %q", r, ReasonReady)
	}
	if got.Status.LastAttemptedRevisions[ns+"/dashboards-artifact"] != rev {
		t.Fatalf("lastAttemptedRevisions = %#v", got.Status.LastAttemptedRevisions)
	}
	if got.Status.LastAppliedRevisions[ns+"/dashboards-artifact"] != rev {
		t.Fatalf("lastAppliedRevisions = %#v", got.Status.LastAppliedRevisions)
	}
	if len(got.Status.Stages) != 1 || got.Status.Stages[0].Phase != stagesv1.StageReady {
		t.Fatalf("stage status = %#v", got.Status.Stages)
	}
}

func TestReconcile_DirectExternalArtifact(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)

	// A direct ExternalArtifact reference (no producer kind).
	rev := servedArtifact(t, c, ns, "bundle", "",
		map[string]string{"cm.yaml": configMapManifest(ns, "from-direct")})

	ss := newStageSet(t, c, ns, "direct", stagesv1.SourceReference{Name: "bundle"})
	reconcileOnce(t, c, ss)

	var deployed corev1.ConfigMap
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "from-direct"}, &deployed); err != nil {
		t.Fatalf("direct artifact ConfigMap was not applied: %v", err)
	}
	if got := getStageSet(t, c, ns, "direct"); got.Status.LastAppliedRevisions[ns+"/bundle"] != rev {
		t.Fatalf("direct applied revision = %#v", got.Status.LastAppliedRevisions)
	}
}

func repointArtifact(t *testing.T, c client.Client, ns, eaName string, files map[string]string) string {
	t.Helper()
	tarball := makeTarGz(t, files)
	sum := sha256.Sum256(tarball)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(tarball)
	}))
	t.Cleanup(srv.Close)
	revision := "rev2@" + digest

	ctx := context.Background()
	ea := &unstructured.Unstructured{}
	ea.SetGroupVersionKind(externalArtifactGVK)
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: eaName}, ea); err != nil {
		t.Fatalf("get ExternalArtifact: %v", err)
	}
	_ = unstructured.SetNestedMap(ea.Object, map[string]any{
		"url": srv.URL, "revision": revision, "digest": digest,
	}, "status", "artifact")
	if err := c.Status().Update(ctx, ea); err != nil {
		t.Fatalf("update ExternalArtifact: %v", err)
	}
	return revision
}

func cmExists(t *testing.T, c client.Client, ns, name string) bool {
	t.Helper()
	var cm corev1.ConfigMap
	err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &cm)
	switch {
	case err == nil:
		return true
	case apierrors.IsNotFound(err):
		return false
	default:
		t.Fatalf("get ConfigMap %s: %v", name, err)
		return false
	}
}

func inventoryEntryCount(t *testing.T, c client.Client, ns, ssName, stage string) int {
	t.Helper()
	var list stagesv1.StageInventoryList
	if err := c.List(context.Background(), &list, client.InNamespace(ns), client.MatchingLabels{
		stagesv1.StageSetLabel: ssName,
		stagesv1.StageLabel:    stage,
	}); err != nil {
		t.Fatalf("list StageInventory: %v", err)
	}
	total := 0
	for i := range list.Items {
		total += len(list.Items[i].Spec.Entries)
	}
	return total
}

// A second run whose artifact dropped an object prunes it from the cluster and
// shrinks the StageInventory.
func TestReconcile_PrunesRemovedObjects(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)

	servedArtifact(t, c, ns, "bundle", "", map[string]string{
		"a.yaml": configMapManifest(ns, "cm-a"),
		"b.yaml": configMapManifest(ns, "cm-b"),
	})
	ss := newStageSet(t, c, ns, "pruner", stagesv1.SourceReference{Name: "bundle"})
	reconcileOnce(t, c, ss)

	if !cmExists(t, c, ns, "cm-a") || !cmExists(t, c, ns, "cm-b") {
		t.Fatal("first run should apply both ConfigMaps")
	}
	if n := inventoryEntryCount(t, c, ns, "pruner", "stage-a"); n != 2 {
		t.Fatalf("inventory after run 1 = %d entries, want 2", n)
	}

	// Run 2: the artifact now contains only cm-a.
	repointArtifact(t, c, ns, "bundle", map[string]string{"a.yaml": configMapManifest(ns, "cm-a")})
	reconcileOnce(t, c, ss)

	if !cmExists(t, c, ns, "cm-a") {
		t.Fatal("cm-a should survive the second run")
	}
	if cmExists(t, c, ns, "cm-b") {
		t.Fatal("cm-b should have been pruned")
	}
	if n := inventoryEntryCount(t, c, ns, "pruner", "stage-a"); n != 1 {
		t.Fatalf("inventory after run 2 = %d entries, want 1", n)
	}
}

// Deleting a StageSet tears its applied objects down via the finalizer, then
// the object is removed.
func TestReconcile_FinalizerTeardown(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	servedArtifact(t, c, ns, "bundle", "", map[string]string{"cm.yaml": configMapManifest(ns, "owned")})
	ss := newStageSet(t, c, ns, "teardown", stagesv1.SourceReference{Name: "bundle"})
	reconcileOnce(t, c, ss)
	if !cmExists(t, c, ns, "owned") {
		t.Fatal("first run should apply the object")
	}

	if err := c.Delete(context.Background(), getStageSet(t, c, ns, "teardown")); err != nil {
		t.Fatalf("delete StageSet: %v", err)
	}
	reconcileOnce(t, c, ss) // reconcileDelete

	if cmExists(t, c, ns, "owned") {
		t.Fatal("owned object should be torn down on deletion")
	}
	var gone stagesv1.StageSet
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "teardown"}, &gone); !apierrors.IsNotFound(err) {
		t.Fatalf("StageSet should be deleted after teardown, got err=%v", err)
	}
}

// Removing a stage from the spec tears down that stage's objects and inventory.
func TestReconcile_OrphanStageTeardown(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	servedArtifact(t, c, ns, "ea-a", "", map[string]string{"a.yaml": configMapManifest(ns, "obj-a")})
	servedArtifact(t, c, ns, "ea-b", "", map[string]string{"b.yaml": configMapManifest(ns, "obj-b")})

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "multi"},
		Spec: stagesv1.StageSetSpec{
			Interval: metav1.Duration{Duration: time.Minute},
			Stages: []stagesv1.Stage{
				{Name: "stage-a", SourceRef: stagesv1.SourceReference{Name: "ea-a"}},
				{Name: "stage-b", SourceRef: stagesv1.SourceReference{Name: "ea-b"}},
			},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	reconcileOnce(t, c, ss)
	if !cmExists(t, c, ns, "obj-a") || !cmExists(t, c, ns, "obj-b") {
		t.Fatal("both stages should apply on the first run")
	}

	// Drop stage-b from the spec.
	live := getStageSet(t, c, ns, "multi")
	live.Spec.Stages = live.Spec.Stages[:1]
	if err := c.Update(context.Background(), live); err != nil {
		t.Fatalf("update StageSet: %v", err)
	}
	reconcileOnce(t, c, ss)

	if !cmExists(t, c, ns, "obj-a") {
		t.Fatal("surviving stage's object should remain")
	}
	if cmExists(t, c, ns, "obj-b") {
		t.Fatal("removed stage's object should be torn down")
	}
	if n := inventoryEntryCount(t, c, ns, "multi", "stage-b"); n != 0 {
		t.Fatalf("removed stage's inventory should be gone, got %d entries", n)
	}
}

// An object that moves from one stage to another in a single spec/content
// change transfers ownership and is NOT pruned.
func TestReconcile_CrossStageOwnershipTransfer(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	servedArtifact(t, c, ns, "ea-a", "", map[string]string{
		"shared.yaml": configMapManifest(ns, "cm-shared"),
		"a.yaml":      configMapManifest(ns, "cm-a"),
	})
	servedArtifact(t, c, ns, "ea-b", "", map[string]string{"b.yaml": configMapManifest(ns, "cm-b")})

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "mover"},
		Spec: stagesv1.StageSetSpec{
			Interval: metav1.Duration{Duration: time.Minute},
			Stages: []stagesv1.Stage{
				{Name: "stage-a", SourceRef: stagesv1.SourceReference{Name: "ea-a"}},
				{Name: "stage-b", SourceRef: stagesv1.SourceReference{Name: "ea-b"}},
			},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	reconcileOnce(t, c, ss)
	if !cmExists(t, c, ns, "cm-shared") || !cmExists(t, c, ns, "cm-a") || !cmExists(t, c, ns, "cm-b") {
		t.Fatal("first run should apply all objects")
	}

	// Move cm-shared from stage-a to stage-b in one change.
	repointArtifact(t, c, ns, "ea-a", map[string]string{"a.yaml": configMapManifest(ns, "cm-a")})
	repointArtifact(t, c, ns, "ea-b", map[string]string{
		"b.yaml":      configMapManifest(ns, "cm-b"),
		"shared.yaml": configMapManifest(ns, "cm-shared"),
	})
	reconcileOnce(t, c, ss)

	if !cmExists(t, c, ns, "cm-shared") {
		t.Fatal("an object moved between stages must transfer ownership, not be pruned")
	}
}

func TestReconcile_SourceNotReady(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	plantExternalArtifact(t, c, ns, "pending-artifact", "dashboards", "", "", false)

	ss := newStageSet(t, c, ns, "waiting", stagesv1.SourceReference{
		APIVersion: "jaas.metio.wtf/v1", Kind: "JsonnetSnippet", Name: "dashboards",
	})
	reconcileOnce(t, c, ss)

	if r := readyReason(getStageSet(t, c, ns, "waiting")); r != ReasonSourceNotReady {
		t.Fatalf("Ready reason = %q, want %q", r, ReasonSourceNotReady)
	}
}

func TestReconcile_ArtifactNotFound(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)

	ss := newStageSet(t, c, ns, "absent", stagesv1.SourceReference{
		APIVersion: "jaas.metio.wtf/v1", Kind: "JsonnetSnippet", Name: "nope",
	})
	reconcileOnce(t, c, ss)

	if r := readyReason(getStageSet(t, c, ns, "absent")); r != ReasonArtifactNotFound {
		t.Fatalf("Ready reason = %q, want %q", r, ReasonArtifactNotFound)
	}
}

func TestReconcile_Suspended(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "paused"},
		Spec: stagesv1.StageSetSpec{
			Interval: metav1.Duration{Duration: 5 * time.Minute},
			Suspend:  true,
			Stages:   []stagesv1.Stage{{Name: "stage-a", SourceRef: stagesv1.SourceReference{Name: "whatever"}}},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	reconcileOnce(t, c, ss)

	if r := readyReason(getStageSet(t, c, ns, "paused")); r != ReasonSuspended {
		t.Fatalf("Ready reason = %q, want %q", r, ReasonSuspended)
	}
}
