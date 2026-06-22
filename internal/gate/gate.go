// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

// Package gate serves the read-only stage-gate endpoint:
//
//	GET /gate/{namespace}/{stageset}/{stage}
//
// It speaks two dialects so two different progressive-delivery models can consume
// it directly:
//
//   - Plain text (the default, and what Flagger sends): 200 when the stage is
//     Ready at the currently pinned revision, 403 otherwise. Flagger's
//     confirm-rollout / confirm-promotion webhooks gate on the HTTP status.
//   - JSON (Accept: application/json): always 200 with a {"ready": bool, …} body.
//     This is the shape Argo Rollouts' web metric expects — it parses the body
//     (successCondition: result.ready == true) and treats a non-2xx as an error,
//     so readiness has to live in the body, not the status.
//
// The response discloses only the stage's phase and pinned revision — never the
// free-form status message, which can carry build, decryption, or RBAC error
// detail — so the endpoint is safe to expose unauthenticated (optionally fenced
// by NetworkPolicy).
package gate

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

// Handler answers stage-gate queries against a (cached) client.
type Handler struct {
	Client client.Client
	// Logger records apiserver-read failures (surfaced to clients as a leak-safe
	// 403) and recovered handler panics, which are otherwise invisible — the
	// response body deliberately discloses nothing. A nil Logger uses
	// slog.Default().
	Logger *slog.Logger
}

// log returns the configured logger or the process default.
func (h *Handler) log() *slog.Logger {
	if h.Logger != nil {
		return h.Logger
	}
	return slog.Default()
}

// gateResult is the JSON body returned to clients that request application/json.
// Ready is the single field a gate evaluates; phase and revision are low-info
// diagnostics. The stage's free-form status message is deliberately NOT included:
// it can carry build, decryption, fetch, or RBAC error detail (file paths, source
// URLs, ServiceAccount names), and this endpoint is unauthenticated.
type gateResult struct {
	Ready     bool   `json:"ready"`
	Namespace string `json:"namespace"`
	StageSet  string `json:"stageset"`
	Stage     string `json:"stage"`
	Phase     string `json:"phase,omitempty"`
	Revision  string `json:"revision,omitempty"`
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	// A panic in a handler goroutine would otherwise crash the gate server (and,
	// before isolation, the manager). Recover, log, and answer leak-safe.
	defer func() {
		if rec := recover(); rec != nil {
			h.log().Error("gate handler panicked", "panic", rec, "path", req.URL.Path)
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
	}()
	if req.Method != http.MethodGet {
		// RFC 7231 §6.5.5 requires a 405 to advertise the supported methods.
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rest := strings.Trim(strings.TrimPrefix(req.URL.Path, "/gate/"), "/")
	parts := strings.Split(rest, "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		http.Error(w, "expected /gate/{namespace}/{stageset}/{stage}", http.StatusBadRequest)
		return
	}
	namespace, name, stage := parts[0], parts[1], parts[2]
	res := gateResult{Namespace: namespace, StageSet: name, Stage: stage}

	var ss stagesv1.StageSet
	if err := h.Client.Get(req.Context(), types.NamespacedName{Namespace: namespace, Name: name}, &ss); err != nil {
		// Deny without distinguishing not-found from other errors (leak-safe). An
		// apiserver-down or RBAC failure looks the same to the client as a clean
		// not-found, so log it — otherwise a degraded gate is silent.
		h.log().Warn("gate stageset lookup failed", "namespace", namespace, "stageset", name, "error", err)
		respond(w, req, res, http.StatusForbidden, "stageset %s/%s is not gateable\n", namespace, name)
		return
	}

	for _, s := range ss.Status.Stages {
		if s.Name != stage {
			continue
		}
		// Only the phase and pinned revision are disclosed — never s.Message,
		// which can carry build/decrypt/RBAC error detail on this unauthenticated
		// endpoint. The full message stays on the StageSet status (RBAC-gated).
		res.Phase, res.Revision = string(s.Phase), s.AppliedRevision
		if s.Phase == stagesv1.StageReady {
			res.Ready = true
			respond(w, req, res, http.StatusOK, "stage %q is Ready at %s\n", stage, s.AppliedRevision)
			return
		}
		respond(w, req, res, http.StatusForbidden, "stage %q is not Ready (phase=%s)\n", stage, s.Phase)
		return
	}
	respond(w, req, res, http.StatusForbidden, "stage %q not found in stageset %s/%s\n", stage, namespace, name)
}

// respond renders one result in whichever dialect the caller asked for. JSON
// callers always get 200 with the readiness in the body; plain-text callers get
// textStatus (200 Ready / 403 not) with the diagnostic message.
func respond(w http.ResponseWriter, req *http.Request, res gateResult, textStatus int, textFormat string, textArgs ...any) {
	if wantsJSON(req) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(res)
		return
	}
	writeText(w, textStatus, textFormat, textArgs...)
}

// wantsJSON reports whether the caller accepts a JSON response.
func wantsJSON(req *http.Request) bool {
	return strings.Contains(req.Header.Get("Accept"), "application/json")
}

// writeText sends a plain-text response. text/plain is never interpreted as
// HTML, so reflecting URL params / stage phase carries no XSS risk.
func writeText(w http.ResponseWriter, status int, format string, args ...any) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	// #nosec G705 -- plain-text endpoint (Content-Type above), not HTML.
	_, _ = fmt.Fprintf(w, format, args...)
}
