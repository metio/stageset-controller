// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package cli

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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

func TestReconcile_WithSource(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "recsrc")
	makeStageSet(t, c, ns, "app") // its stage sourceRef is "app-artifact"

	ea := &unstructured.Unstructured{}
	ea.SetGroupVersionKind(artifact.ExternalArtifactGVK)
	ea.SetNamespace(ns)
	ea.SetName("app-artifact")
	if err := c.Create(context.Background(), ea); err != nil {
		t.Fatalf("create ExternalArtifact: %v", err)
	}

	_, stderr, code := runCLI(t, cfg, "reconcile", "app", "-n", ns, "--with-source")
	if code != exitOK {
		t.Fatalf("reconcile --with-source exit = %d (stderr=%s)", code, stderr)
	}

	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(artifact.ExternalArtifactGVK)
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "app-artifact"}, got); err != nil {
		t.Fatalf("get ExternalArtifact: %v", err)
	}
	if got.GetAnnotations()[requestedAtAnnotation] == "" {
		t.Errorf("source ExternalArtifact not annotated with requestedAt: %v", got.GetAnnotations())
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

	ea := &unstructured.Unstructured{}
	ea.SetGroupVersionKind(artifact.ExternalArtifactGVK)
	ea.SetNamespace(ns)
	ea.SetName("app-artifact")
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
}
