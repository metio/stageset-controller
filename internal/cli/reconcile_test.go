// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package cli

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/artifact"
)

func getAnnotations(t *testing.T, c client.Client, ns, name string) map[string]string {
	t.Helper()
	var ss stagesv1.StageSet
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &ss); err != nil {
		t.Fatalf("get StageSet: %v", err)
	}
	return ss.GetAnnotations()
}

func TestReconcile_StampsRequestedAt(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "rec")
	makeStageSet(t, c, ns, "app")

	stdout, stderr, code := runCLI(t, cfg, "reconcile", "app", "-n", ns)
	if code != exitOK {
		t.Fatalf("reconcile exit = %d (stderr=%s)", code, stderr)
	}
	if !strings.Contains(stdout, "Reconcile requested") {
		t.Errorf("missing confirmation:\n%s", stdout)
	}
	ann := getAnnotations(t, c, ns, "app")
	if ann[requestedAtAnnotation] == "" {
		t.Errorf("requestedAt annotation not set: %v", ann)
	}
}

func TestReconcile_SingleStage(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "recstage")
	makeStageSet(t, c, ns, "app")

	_, stderr, code := runCLI(t, cfg, "reconcile", "app", "-n", ns, "--stage", "first")
	if code != exitOK {
		t.Fatalf("reconcile --stage exit = %d (stderr=%s)", code, stderr)
	}
	ann := getAnnotations(t, c, ns, "app")
	val := ann[reconcileStageAnnotation]
	if !strings.HasPrefix(val, "first@") {
		t.Errorf("reconcile-stage annotation = %q, want first@<token>", val)
	}
}

func TestReconcile_UpdateNow(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "recnow")
	makeStageSet(t, c, ns, "app")

	_, _, code := runCLI(t, cfg, "reconcile", "app", "-n", ns, "--update-now")
	if code != exitOK {
		t.Fatalf("reconcile --update-now exit = %d", code)
	}
	ann := getAnnotations(t, c, ns, "app")
	if ann[updateNowAnnotation] == "" {
		t.Errorf("update-now annotation not set: %v", ann)
	}
	if ann[updateNowAnnotation] != ann[requestedAtAnnotation] {
		t.Errorf("update-now token should match requestedAt token")
	}
}

func TestReconcile_BudgetOverride(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "recbudget")
	makeStageSet(t, c, ns, "app")

	_, _, code := runCLI(t, cfg, "reconcile", "app", "-n", ns, "--budget-override")
	if code != exitOK {
		t.Fatalf("reconcile --budget-override exit = %d", code)
	}
	ann := getAnnotations(t, c, ns, "app")
	if ann[budgetOverrideAnnotation] == "" {
		t.Errorf("budget-override annotation not set: %v", ann)
	}
	if ann[budgetOverrideAnnotation] != ann[requestedAtAnnotation] {
		t.Errorf("budget-override token should match requestedAt token")
	}
}

func TestReconcile_UnknownStageIsError(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "recbad")
	makeStageSet(t, c, ns, "app")

	_, stderr, code := runCLI(t, cfg, "reconcile", "app", "-n", ns, "--stage", "ghost")
	if code != exitError {
		t.Fatalf("unknown stage exit = %d, want %d", code, exitError)
	}
	if !strings.Contains(stderr, "not found") {
		t.Errorf("stderr missing 'not found':\n%s", stderr)
	}
}

func TestReconcile_SuspendedRefusedWithoutForce(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "recsusp")
	ss := makeStageSet(t, c, ns, "app")
	ss.Spec.Suspend = true
	if err := c.Update(context.Background(), ss); err != nil {
		t.Fatalf("suspend: %v", err)
	}

	_, stderr, code := runCLI(t, cfg, "reconcile", "app", "-n", ns)
	if code != exitError {
		t.Fatalf("suspended reconcile exit = %d, want %d", code, exitError)
	}
	if !strings.Contains(stderr, "suspended") {
		t.Errorf("stderr missing suspend warning:\n%s", stderr)
	}

	// --force proceeds anyway.
	_, _, code = runCLI(t, cfg, "reconcile", "app", "-n", ns, "--force")
	if code != exitOK {
		t.Fatalf("reconcile --force on suspended exit = %d", code)
	}
}

// --with-source must annotate the PRODUCER behind the stage's ExternalArtifact
// (nothing acts on requestedAt on the EA itself — the EA is the producer's
// output). The stage's EA carries a back-pointer to a JsonnetSnippet; that
// snippet is what gets stamped.
func TestReconcile_WithSource(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "recsrc")
	makeStageSet(t, c, ns, "app") // its stage sourceRef is "app-artifact"

	// The producer snippet, and the EA it publishes with a back-pointer to it.
	snip := &unstructured.Unstructured{}
	snip.SetGroupVersionKind(schema.GroupVersionKind{Group: "jaas.metio.wtf", Version: "v1", Kind: "JsonnetSnippet"})
	snip.SetNamespace(ns)
	snip.SetName("dash")
	if err := c.Create(context.Background(), snip); err != nil {
		t.Fatalf("create JsonnetSnippet: %v", err)
	}
	ea := &unstructured.Unstructured{}
	ea.SetGroupVersionKind(artifact.ExternalArtifactGVK)
	ea.SetNamespace(ns)
	ea.SetName("app-artifact")
	_ = unstructured.SetNestedStringMap(ea.Object, map[string]string{
		"apiVersion": "jaas.metio.wtf/v1", "kind": "JsonnetSnippet", "name": "dash",
	}, "spec", "sourceRef")
	if err := c.Create(context.Background(), ea); err != nil {
		t.Fatalf("create ExternalArtifact: %v", err)
	}

	_, stderr, code := runCLI(t, cfg, "reconcile", "app", "-n", ns, "--with-source")
	if code != exitOK {
		t.Fatalf("reconcile --with-source exit = %d (stderr=%s)", code, stderr)
	}

	// The PRODUCER snippet is annotated…
	gotSnip := &unstructured.Unstructured{}
	gotSnip.SetGroupVersionKind(schema.GroupVersionKind{Group: "jaas.metio.wtf", Version: "v1", Kind: "JsonnetSnippet"})
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "dash"}, gotSnip); err != nil {
		t.Fatalf("get JsonnetSnippet: %v", err)
	}
	if gotSnip.GetAnnotations()[requestedAtAnnotation] == "" {
		t.Errorf("producer snippet not annotated with requestedAt: %v", gotSnip.GetAnnotations())
	}
	// …and the EA itself is NOT (annotating it is the no-op the fix removes).
	gotEA := &unstructured.Unstructured{}
	gotEA.SetGroupVersionKind(artifact.ExternalArtifactGVK)
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "app-artifact"}, gotEA); err != nil {
		t.Fatalf("get ExternalArtifact: %v", err)
	}
	if gotEA.GetAnnotations()[requestedAtAnnotation] != "" {
		t.Errorf("the EA itself must not be annotated — nothing acts on requestedAt there: %v", gotEA.GetAnnotations())
	}
}

// An ExternalArtifact with no producer back-pointer is a no-op for
// --with-source: nothing re-publishes it. It must warn (and count as failed
// under --strict) rather than silently annotate a CR no controller watches.
func TestReconcile_WithSource_ProducerlessEAWarns(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "recnoproducer")
	makeStageSet(t, c, ns, "app")

	ea := &unstructured.Unstructured{}
	ea.SetGroupVersionKind(artifact.ExternalArtifactGVK)
	ea.SetNamespace(ns)
	ea.SetName("app-artifact") // no spec.sourceRef back-pointer
	if err := c.Create(context.Background(), ea); err != nil {
		t.Fatalf("create ExternalArtifact: %v", err)
	}

	_, stderr, code := runCLI(t, cfg, "reconcile", "app", "-n", ns, "--with-source", "--strict")
	if code == exitOK {
		t.Fatalf("a producerless EA under --strict must exit non-zero (stderr=%s)", stderr)
	}
	if !strings.Contains(stderr, "without a producer back-pointer") {
		t.Errorf("expected a producer-back-pointer warning, got: %s", stderr)
	}
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(artifact.ExternalArtifactGVK)
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "app-artifact"}, got); err != nil {
		t.Fatalf("get ExternalArtifact: %v", err)
	}
	if got.GetAnnotations()[requestedAtAnnotation] != "" {
		t.Errorf("a producerless EA must not be annotated (it would be a silent no-op): %v", got.GetAnnotations())
	}
}

// TestReconcile_WithSourceStrictFails proves --strict turns an unresolvable
// source into a non-zero exit, instead of warning and exiting 0. The
// ExternalArtifact is deliberately not created, so its Get fails.
func TestReconcile_WithSourceStrictFails(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "recstrict")
	makeStageSet(t, c, ns, "app") // sourceRef "app-artifact" is never created

	_, stderr, code := runCLI(t, cfg, "reconcile", "app", "-n", ns, "--with-source", "--strict")
	if code == exitOK {
		t.Fatalf("--strict with an unresolvable source should exit non-zero (stderr=%s)", stderr)
	}
	if !strings.Contains(stderr, "warning: cannot reconcile source") {
		t.Errorf("expected a per-source warning on stderr, got: %s", stderr)
	}
}

// TestReconcile_WithSourceStrictSucceeds proves --strict exits 0 when every
// source resolves, so the flag does not break the happy path.
func TestReconcile_WithSourceStrictSucceeds(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "recstrictok")
	makeStageSet(t, c, ns, "app")

	snip := &unstructured.Unstructured{}
	snip.SetGroupVersionKind(schema.GroupVersionKind{Group: "jaas.metio.wtf", Version: "v1", Kind: "JsonnetSnippet"})
	snip.SetNamespace(ns)
	snip.SetName("dash")
	if err := c.Create(context.Background(), snip); err != nil {
		t.Fatalf("create JsonnetSnippet: %v", err)
	}
	ea := &unstructured.Unstructured{}
	ea.SetGroupVersionKind(artifact.ExternalArtifactGVK)
	ea.SetNamespace(ns)
	ea.SetName("app-artifact")
	_ = unstructured.SetNestedStringMap(ea.Object, map[string]string{
		"apiVersion": "jaas.metio.wtf/v1", "kind": "JsonnetSnippet", "name": "dash",
	}, "spec", "sourceRef")
	if err := c.Create(context.Background(), ea); err != nil {
		t.Fatalf("create ExternalArtifact: %v", err)
	}

	_, stderr, code := runCLI(t, cfg, "reconcile", "app", "-n", ns, "--with-source", "--strict")
	if code != exitOK {
		t.Fatalf("--strict with all sources resolvable should exit 0 (code=%d stderr=%s)", code, stderr)
	}
}

func TestReconcileHandled(t *testing.T) {
	mk := func(handled string, stages ...stagesv1.StageStatus) *stagesv1.StageSet {
		ss := &stagesv1.StageSet{}
		ss.Status.SetLastHandledReconcileRequest(handled)
		ss.Status.Stages = stages
		return ss
	}

	if !reconcileHandled(mk("tok"), reconcileOptions{}, "tok") {
		t.Error("whole-object: matching token should be handled")
	}
	if reconcileHandled(mk("old"), reconcileOptions{}, "tok") {
		t.Error("whole-object: stale token should not be handled")
	}

	stageHandled := mk("", stagesv1.StageStatus{Name: "first", LastHandledReconcileAt: "tok"})
	if !reconcileHandled(stageHandled, reconcileOptions{stage: "first"}, "tok") {
		t.Error("single-stage: matching stage token should be handled")
	}
	if reconcileHandled(stageHandled, reconcileOptions{stage: "first"}, "other") {
		t.Error("single-stage: stale stage token should not be handled")
	}

	withPending := mk("tok")
	withPending.Status.PendingUpdate = &stagesv1.PendingUpdate{}
	if reconcileHandled(withPending, reconcileOptions{updateNow: true}, "tok") {
		t.Error("update-now: a still-pending update means not handled")
	}

	// Stage-scoped + update-now must also wait for the held update to apply.
	stagePending := mk("", stagesv1.StageStatus{Name: "first", LastHandledReconcileAt: "tok"})
	stagePending.Status.PendingUpdate = &stagesv1.PendingUpdate{}
	if reconcileHandled(stagePending, reconcileOptions{stage: "first", updateNow: true}, "tok") {
		t.Error("single-stage update-now: a still-pending update means not handled")
	}
}
