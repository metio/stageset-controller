// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package gate

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

// panickingClient embeds client.Client so it satisfies the interface, but its Get
// panics — exercising ServeHTTP's deferred recover so a handler-goroutine panic
// turns into a leak-safe 500 instead of crashing the gate server.
type panickingClient struct {
	client.Client
}

func (panickingClient) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	panic("simulated apiserver client panic")
}

// TestGate_RecoversPanic covers the deferred recover() in ServeHTTP: a panic
// during the lookup is logged and answered with a 500, with no panic detail in
// the body.
func TestGate_RecoversPanic(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h := &Handler{Client: panickingClient{}, Logger: logger}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/gate/flux-system/platform/migrations", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("panic recovery status = %d, want 500", rec.Code)
	}
	if !strings.Contains(buf.String(), "gate handler panicked") {
		t.Fatalf("panic should be logged, got: %q", buf.String())
	}
	// The leak-safe body must not echo the panic value.
	if strings.Contains(rec.Body.String(), "simulated apiserver client panic") {
		t.Fatalf("500 body leaked the panic value: %s", rec.Body.String())
	}
}

// TestGate_SkipsNonMatchingStages covers the `s.Name != stage` continue branch:
// the handler must scan past earlier, non-matching stages and resolve the
// requested one.
func TestGate_SkipsNonMatchingStages(t *testing.T) {
	t.Parallel()
	h := &Handler{Client: gateClient(t, stageSet("flux-system", "platform",
		stagesv1.StageStatus{Name: "migrations", Phase: stagesv1.StageApplying},
		stagesv1.StageStatus{Name: "frontend", Phase: stagesv1.StageReady}))}

	// The match is the second entry, so the first must be skipped.
	if code := gateCode(t, h, http.MethodGet, "/gate/flux-system/platform/frontend"); code != http.StatusOK {
		t.Fatalf("ready second-stage gate = %d, want 200", code)
	}
	// The first stage still resolves to its own (unready) verdict.
	if code := gateCode(t, h, http.MethodGet, "/gate/flux-system/platform/migrations"); code != http.StatusForbidden {
		t.Fatalf("unready first-stage gate = %d, want 403", code)
	}
}
