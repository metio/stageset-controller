// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

// Package gate serves the read-only Flagger stage-gate endpoint:
//
//	GET /gate/{namespace}/{stageset}/{stage}
//	  200 — the stage is Ready at the currently pinned revision
//	  403 — otherwise (body carries the phase + message for debugging)
//
// Flagger's confirm-rollout / confirm-promotion webhooks block until the URL
// returns 200, letting one StageSet gate another's Canary promotion. The
// response leaks only a stage phase, so the endpoint is safe to expose
// unauthenticated (optionally fenced by NetworkPolicy).
package gate

import (
	"fmt"
	"net/http"
	"strings"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

// Handler answers stage-gate queries against a (cached) client.
type Handler struct {
	Client client.Client
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
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

	var ss stagesv1.StageSet
	if err := h.Client.Get(req.Context(), types.NamespacedName{Namespace: namespace, Name: name}, &ss); err != nil {
		// Deny without distinguishing not-found from other errors (leak-safe).
		writeText(w, http.StatusForbidden, "stageset %s/%s is not gateable\n", namespace, name)
		return
	}

	for _, s := range ss.Status.Stages {
		if s.Name != stage {
			continue
		}
		if s.Phase == stagesv1.StageReady {
			writeText(w, http.StatusOK, "stage %q is Ready at %s\n", stage, s.AppliedRevision)
			return
		}
		writeText(w, http.StatusForbidden, "stage %q phase=%s: %s\n", stage, s.Phase, s.Message)
		return
	}
	writeText(w, http.StatusForbidden, "stage %q not found in stageset %s/%s\n", stage, namespace, name)
}

// writeText sends a plain-text response. text/plain is never interpreted as
// HTML, so reflecting URL params / stage phase carries no XSS risk.
func writeText(w http.ResponseWriter, status int, format string, args ...any) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	// #nosec G705 -- plain-text endpoint (Content-Type above), not HTML.
	_, _ = fmt.Fprintf(w, format, args...)
}
