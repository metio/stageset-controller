/*
 * SPDX-FileCopyrightText: The stageset-controller Authors
 * SPDX-License-Identifier: 0BSD
 */

// Package mcp exposes the controller's StageSet introspection (and, when opted
// in, control) as Model Context Protocol tools so an LLM agent can read status
// and drive reconciliation directly. The tools are thin adapters over the
// StageSet API types, reading and patching as the controller's ServiceAccount,
// so an agent can never exceed the controller's own RBAC.
//
// The server is in-cluster only — it serves over streamable HTTP from the
// controller pod (there is no cluster-free mode, unlike a renderer).
package mcp

import (
	"log/slog"
	"net/http"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Config carries the static, server-lifetime knobs.
type Config struct {
	// KubeClient reads (and, with AllowMutations, patches) StageSet resources.
	// The embedded server passes the manager's client.
	KubeClient client.Client

	// RunbookBaseURL is the docs-site prefix for per-reason remediation pages,
	// used to build the runbook link in get_stageset. Empty omits the link.
	RunbookBaseURL string

	// AllowMutations registers the gated write tools (reconcile/suspend/resume)
	// in addition to the read tools. Off by default.
	AllowMutations bool

	// Version is reported to MCP clients as the server implementation version.
	Version string

	// Logger receives the SDK's server-activity logs; it is the controller's
	// shared logger so MCP diagnostics match every other surface.
	Logger *slog.Logger
}

// NewServer builds the MCP server with the StageSet tool catalog registered.
func NewServer(cfg Config) *mcpsdk.Server {
	server := mcpsdk.NewServer(
		&mcpsdk.Implementation{Name: "stageset-controller", Version: cfg.Version},
		&mcpsdk.ServerOptions{Logger: cfg.Logger},
	)
	if cfg.KubeClient != nil {
		registerStageSetTools(server, cfg)
		// Write tools are a further opt-in on top of having a client.
		if cfg.AllowMutations {
			registerMutationTools(server, cfg)
		}
	}
	return server
}

// NewHTTPHandler builds an http.Handler serving the MCP streamable-HTTP
// transport backed by a single server with the configured tool catalog. The
// controller mounts this on its own listener; the server instance is reused
// across sessions.
func NewHTTPHandler(cfg Config) http.Handler {
	server := NewServer(cfg)
	return mcpsdk.NewStreamableHTTPHandler(
		func(*http.Request) *mcpsdk.Server { return server },
		&mcpsdk.StreamableHTTPOptions{Logger: cfg.Logger},
	)
}

// errorResult builds an MCP tool-error result carrying msg as its text content.
func errorResult(msg string) *mcpsdk.CallToolResult {
	return &mcpsdk.CallToolResult{
		IsError: true,
		Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: msg}},
	}
}
