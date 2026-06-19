// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package gate

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

func gateClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	if err := stagesv1.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
}

func stageSet(ns, name string, stages ...stagesv1.StageStatus) *stagesv1.StageSet {
	return &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Status:     stagesv1.StageSetStatus{Stages: stages},
	}
}

func gateCode(t *testing.T, h http.Handler, method, path string) int {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec.Code
}

func TestGate_ReadyStage(t *testing.T) {
	t.Parallel()
	h := &Handler{Client: gateClient(t, stageSet("flux-system", "platform",
		stagesv1.StageStatus{Name: "migrations", Phase: stagesv1.StageReady}))}
	if code := gateCode(t, h, http.MethodGet, "/gate/flux-system/platform/migrations"); code != http.StatusOK {
		t.Fatalf("ready stage gate = %d, want 200", code)
	}
}

func TestGate_UnreadyStage(t *testing.T) {
	t.Parallel()
	h := &Handler{Client: gateClient(t, stageSet("flux-system", "platform",
		stagesv1.StageStatus{Name: "migrations", Phase: stagesv1.StageApplying}))}
	if code := gateCode(t, h, http.MethodGet, "/gate/flux-system/platform/migrations"); code != http.StatusForbidden {
		t.Fatalf("unready stage gate = %d, want 403", code)
	}
}

func TestGate_UnknownStage(t *testing.T) {
	t.Parallel()
	h := &Handler{Client: gateClient(t, stageSet("flux-system", "platform"))}
	if code := gateCode(t, h, http.MethodGet, "/gate/flux-system/platform/nope"); code != http.StatusForbidden {
		t.Fatalf("unknown stage gate = %d, want 403", code)
	}
}

func TestGate_MissingStageSet(t *testing.T) {
	t.Parallel()
	h := &Handler{Client: gateClient(t)}
	if code := gateCode(t, h, http.MethodGet, "/gate/flux-system/nope/x"); code != http.StatusForbidden {
		t.Fatalf("missing stageset gate = %d, want 403", code)
	}
}

func TestGate_BadPath(t *testing.T) {
	t.Parallel()
	h := &Handler{Client: gateClient(t)}
	if code := gateCode(t, h, http.MethodGet, "/gate/only/two"); code != http.StatusBadRequest {
		t.Fatalf("bad path = %d, want 400", code)
	}
}

func TestGate_MethodNotAllowed(t *testing.T) {
	t.Parallel()
	h := &Handler{Client: gateClient(t)}
	if code := gateCode(t, h, http.MethodPost, "/gate/a/b/c"); code != http.StatusMethodNotAllowed {
		t.Fatalf("POST = %d, want 405", code)
	}
}

// gateJSON issues an Accept: application/json request and returns the decoded
// body plus the response.
func gateJSON(t *testing.T, h http.Handler, path string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Accept", "application/json")
	h.ServeHTTP(rec, req)
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode JSON body %q: %v", rec.Body.String(), err)
	}
	return rec, body
}

func TestGate_JSON_ReadyStage(t *testing.T) {
	t.Parallel()
	h := &Handler{Client: gateClient(t, stageSet("flux-system", "platform",
		stagesv1.StageStatus{Name: "migrations", Phase: stagesv1.StageReady, AppliedRevision: "sha256:abc"}))}
	rec, body := gateJSON(t, h, "/gate/flux-system/platform/migrations")
	// JSON callers always get 200; readiness is in the body, not the status.
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Fatalf("content-type = %q", ct)
	}
	if body["ready"] != true {
		t.Fatalf("ready = %v, want true", body["ready"])
	}
	if body["stage"] != "migrations" || body["revision"] != "sha256:abc" {
		t.Fatalf("unexpected body: %v", body)
	}
}

func TestGate_JSON_UnreadyStageIs200WithReadyFalse(t *testing.T) {
	t.Parallel()
	h := &Handler{Client: gateClient(t, stageSet("flux-system", "platform",
		stagesv1.StageStatus{Name: "migrations", Phase: stagesv1.StageApplying}))}
	rec, body := gateJSON(t, h, "/gate/flux-system/platform/migrations")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (Argo web metric needs 2xx)", rec.Code)
	}
	if body["ready"] != false {
		t.Fatalf("ready = %v, want false", body["ready"])
	}
	if body["phase"] != string(stagesv1.StageApplying) {
		t.Fatalf("phase = %v, want %s", body["phase"], stagesv1.StageApplying)
	}
}

func TestGate_JSON_MissingStageSet(t *testing.T) {
	t.Parallel()
	h := &Handler{Client: gateClient(t)}
	rec, body := gateJSON(t, h, "/gate/flux-system/nope/x")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if body["ready"] != false {
		t.Fatalf("ready = %v, want false", body["ready"])
	}
}

// TestGate_DoesNotLeakStageMessage pins the leak-safety contract: a failed stage's
// free-form status message (which can carry build/decrypt/RBAC error detail) must
// never appear in the unauthenticated gate response, in either dialect. The
// structured phase still reports, so legitimate diagnostics survive.
func TestGate_DoesNotLeakStageMessage(t *testing.T) {
	t.Parallel()
	const sentinel = "kustomize build /tmp/overlays: secret-leak-sentinel"
	h := &Handler{Client: gateClient(t, stageSet("flux-system", "platform",
		stagesv1.StageStatus{Name: "migrations", Phase: stagesv1.StageFailed, Message: sentinel}))}

	// Plain text (Flagger dialect).
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/gate/flux-system/platform/migrations", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("text status = %d, want 403", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "secret-leak-sentinel") {
		t.Errorf("text gate leaked the stage message: %s", rec.Body.String())
	}

	// JSON (Argo dialect).
	jreq := httptest.NewRequest(http.MethodGet, "/gate/flux-system/platform/migrations", nil)
	jreq.Header.Set("Accept", "application/json")
	jrec := httptest.NewRecorder()
	h.ServeHTTP(jrec, jreq)
	if strings.Contains(jrec.Body.String(), "secret-leak-sentinel") {
		t.Errorf("json gate leaked the stage message: %s", jrec.Body.String())
	}
	if !strings.Contains(jrec.Body.String(), string(stagesv1.StageFailed)) {
		t.Errorf("json gate should still report the structured phase: %s", jrec.Body.String())
	}
}

// TestGate_LookupFailureLogs proves an apiserver-read failure (here a clean
// not-found that the client maps to the leak-safe 403) is logged via the
// injected logger, so a degraded gate is not silent.
func TestGate_LookupFailureLogs(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	// No StageSet objects, so the Get returns NotFound.
	h := &Handler{Client: gateClient(t), Logger: logger}
	if code := gateCode(t, h, http.MethodGet, "/gate/flux-system/absent/migrations"); code != http.StatusForbidden {
		t.Fatalf("missing stageset gate = %d, want 403", code)
	}
	if !strings.Contains(buf.String(), "gate stageset lookup failed") {
		t.Fatalf("lookup failure should be logged, got: %q", buf.String())
	}
}
