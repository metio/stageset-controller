// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/metio/stageset-controller/internal/opstate"
)

func TestManagerStatusHandler_AvailableReturnsOK(t *testing.T) {
	state := opstate.New()
	state.MarkAvailable()

	rec := httptest.NewRecorder()
	managerStatusHandler(state)(rec, httptest.NewRequest(http.MethodGet, "/manager", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
}

func TestManagerStatusHandler_UnavailableReturns503WithTheReason(t *testing.T) {
	state := opstate.New()
	state.RecordAttempt()
	state.RecordAttempt()
	state.MarkUnavailable(`create manager: Get "https://10.24.64.1:443/api": i/o timeout`)

	rec := httptest.NewRecorder()
	managerStatusHandler(state)(rec, httptest.NewRequest(http.MethodGet, "/manager", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	var body managerStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v (%q)", err, rec.Body.String())
	}
	if body.Status != "unavailable" {
		t.Errorf("status field = %q, want unavailable", body.Status)
	}
	if body.Reason == "" {
		t.Error("reason field is empty, want the failure that keeps the manager down")
	}
	if body.Attempts != 2 {
		t.Errorf("attempts field = %d, want 2", body.Attempts)
	}
	if body.Since == "" {
		t.Error("since field is empty, want the time the reading last changed")
	}
}

func TestManagerStatusHandler_RejectsNonGET(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			rec := httptest.NewRecorder()
			managerStatusHandler(opstate.New())(rec, httptest.NewRequest(method, "/manager", nil))
			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
			}
			if got := rec.Header().Get("Allow"); got != http.MethodGet {
				t.Errorf("Allow = %q, want %q", got, http.MethodGet)
			}
		})
	}
}

// TestServeProbes_LivenessIsUnconditional is the property that keeps a degraded
// controller from being restarted: /healthz answers 200 while the manager is
// down, so only /readyz withdraws the pod from its Services.
func TestServeProbes_LivenessIsUnconditional(t *testing.T) {
	state := opstate.New()
	addr := "127.0.0.1:" + freePort(t)
	srv, err := serveProbes(addr, state, discardLogger())
	if err != nil {
		t.Fatalf("serveProbes: %v", err)
	}
	t.Cleanup(func() { shutdownServer(srv) })

	if code, _ := probeGet(t, addr, "/healthz"); code != http.StatusOK {
		t.Errorf("GET /healthz = %d, want %d while the manager is down", code, http.StatusOK)
	}
	if code, _ := probeGet(t, addr, "/readyz"); code != http.StatusServiceUnavailable {
		t.Errorf("GET /readyz = %d, want %d while the manager is down", code, http.StatusServiceUnavailable)
	}
	if code, _ := probeGet(t, addr, "/manager"); code != http.StatusServiceUnavailable {
		t.Errorf("GET /manager = %d, want %d while the manager is down", code, http.StatusServiceUnavailable)
	}

	state.MarkAvailable()
	for _, path := range []string{"/healthz", "/readyz", "/manager"} {
		if code, _ := probeGet(t, addr, path); code != http.StatusOK {
			t.Errorf("GET %s = %d, want %d once the manager is available", path, code, http.StatusOK)
		}
	}
}

// A disabled address binds nothing, matching controller-runtime's convention for
// the same flags.
func TestServeProbesAndMetrics_DisabledAddressBindsNothing(t *testing.T) {
	for _, addr := range []string{"", "0"} {
		srv, err := serveProbes(addr, opstate.New(), discardLogger())
		if err != nil || srv != nil {
			t.Errorf("serveProbes(%q) = (%v, %v), want (nil, nil)", addr, srv, err)
		}
		msrv, err := serveMetrics(addr, discardLogger())
		if err != nil || msrv != nil {
			t.Errorf("serveMetrics(%q) = (%v, %v), want (nil, nil)", addr, msrv, err)
		}
	}
}

func TestServeMetrics_ServesTheControllerRuntimeRegistry(t *testing.T) {
	addr := "127.0.0.1:" + freePort(t)
	srv, err := serveMetrics(addr, discardLogger())
	if err != nil {
		t.Fatalf("serveMetrics: %v", err)
	}
	t.Cleanup(func() { shutdownServer(srv) })

	code, body := probeGet(t, addr, "/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics = %d, want %d", code, http.StatusOK)
	}
	// The availability gauge is the reason this endpoint is bound by the binary,
	// so its presence is what the test is really about.
	if !strings.Contains(body, "stageset_manager_available") {
		t.Errorf("/metrics does not expose stageset_manager_available; got %d bytes", len(body))
	}
}

// probeGet returns the status code and body of a GET against an endpoint. A code
// of 0 means the request itself failed, which a caller polling a server that is
// still coming up treats as "not yet" rather than as a fatal error.
func probeGet(t *testing.T, addr, path string) (int, string) {
	t.Helper()
	resp, err := http.Get("http://" + addr + path)
	if err != nil {
		return 0, err.Error()
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}
