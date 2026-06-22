// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package cliflags_test

import (
	"flag"
	"testing"
	"time"

	"github.com/metio/stageset-controller/internal/cliflags"
)

func validFlags(t *testing.T) *cliflags.Flags {
	t.Helper()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	f := cliflags.Register(fs)
	if err := fs.Parse(nil); err != nil {
		t.Fatalf("parse defaults: %v", err)
	}
	return f
}

func TestFlags_Validate_AcceptsDefaults(t *testing.T) {
	if err := validFlags(t).Validate(); err != nil {
		t.Fatalf("defaults should validate, got %v", err)
	}
}

// "0" disables the metrics/probe endpoint, an empty gate address disables the
// gate, and an empty MCP address is ignored while MCP is off — all must validate.
func TestFlags_Validate_AcceptsDisableForms(t *testing.T) {
	f := validFlags(t)
	*f.MetricsAddr = "0"
	*f.ProbeAddr = "0"
	*f.GateAddr = ""
	*f.MCPAddr = "" // not checked while --enable-mcp is false
	if err := f.Validate(); err != nil {
		t.Fatalf("disable forms should validate, got %v", err)
	}
}

func TestFlags_Validate_RejectsInvalid(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*cliflags.Flags)
	}{
		{"metrics missing colon", func(f *cliflags.Flags) { *f.MetricsAddr = "8080" }},
		{"metrics port out of range", func(f *cliflags.Flags) { *f.MetricsAddr = ":70000" }},
		{"probe malformed", func(f *cliflags.Flags) { *f.ProbeAddr = "nope" }},
		{"gate non-empty malformed", func(f *cliflags.Flags) { *f.GateAddr = "localhost" }},
		{"mcp empty when enabled", func(f *cliflags.Flags) { *f.EnableMCP = true; *f.MCPAddr = "" }},
		{"mcp malformed when enabled", func(f *cliflags.Flags) { *f.EnableMCP = true; *f.MCPAddr = ":99999" }},
		{"webhook-port zero", func(f *cliflags.Flags) { *f.WebhookPort = 0 }},
		{"webhook-port too large", func(f *cliflags.Flags) { *f.WebhookPort = 70000 }},
		{"shard-cap zero", func(f *cliflags.Flags) { *f.ShardCap = 0 }},
		{"default-interval zero", func(f *cliflags.Flags) { *f.DefaultInterval = 0 }},
		{"default-interval negative", func(f *cliflags.Flags) { *f.DefaultInterval = -time.Second }},
		{"max-teardown-wait negative", func(f *cliflags.Flags) { *f.MaxTeardownWait = -time.Second }},
		{"webhook-cert-validity zero", func(f *cliflags.Flags) { *f.WebhookCertValidity = 0 }},
		{"sample-ratio below zero", func(f *cliflags.Flags) { *f.TracingSampleRatio = -0.1 }},
		{"sample-ratio above one", func(f *cliflags.Flags) { *f.TracingSampleRatio = 1.5 }},
		{"webhook-cert-mode bogus", func(f *cliflags.Flags) { *f.WebhookCertMode = "vault" }},
		{"rollback s3 sse bogus", func(f *cliflags.Flags) { *f.RBS3SSE = "aes" }},
		{"log-level bogus", func(f *cliflags.Flags) { *f.LogLevel = "trace" }},
		{"log-format bogus", func(f *cliflags.Flags) { *f.LogFormat = "yaml" }},
		{"mcp-allow-mutations without enable-mcp", func(f *cliflags.Flags) { *f.MCPAllowMutations = true }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := validFlags(t)
			tt.mutate(f)
			if err := f.Validate(); err == nil {
				t.Fatalf("%s: expected a validation error, got nil", tt.name)
			}
		})
	}
}
