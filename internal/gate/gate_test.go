// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package gate

import (
	"net/http"
	"net/http/httptest"
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
