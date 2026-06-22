// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package main

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"k8s.io/client-go/rest"

	"github.com/metio/stageset-controller/internal/webhook/selfsigned"
)

// TestRun_HelpReturnsZero covers the flag.ErrHelp branch in run(): asking for
// --help is a successful no-op exit, distinct from a parse error (exit 2).
func TestRun_HelpReturnsZero(t *testing.T) {
	if got := run(context.Background(), []string{"--help"}, nil, io.Discard); got != 0 {
		t.Errorf("run(--help) = %d, want 0", got)
	}
}

// TestRun_InvalidFlagValueReturnsTwo covers the c.Validate() failure branch:
// a syntactically valid flag with an out-of-range value (here a bogus log
// level) parses fine but fails validation, which run maps to exit 2 before any
// cluster contact.
func TestRun_InvalidFlagValueReturnsTwo(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "bad log level", args: []string{"--log-level=trace"}},
		{name: "bad log format", args: []string{"--log-format=yaml"}},
		{name: "tracing sample ratio above one", args: []string{"--tracing-sample-ratio=2"}},
		{name: "tracing sample ratio below zero", args: []string{"--tracing-sample-ratio=-1"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := run(context.Background(), tc.args, nil, io.Discard); got != 2 {
				t.Errorf("run(%v) = %d, want 2", tc.args, got)
			}
		})
	}
}

// TestProvisionSelfSignedWebhookCert_RequiresNamespace covers the early-return
// guard: with no resolvable service namespace (neither flag nor in-cluster
// mount), provisioning fails fast with a descriptive error rather than
// generating a cert against an empty namespace.
func TestProvisionSelfSignedWebhookCert_RequiresNamespace(t *testing.T) {
	done, err := provisionSelfSignedWebhookCert(
		context.Background(),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		&rest.Config{},
		selfsigned.Input{ServiceName: "svc", Namespace: ""},
		t.TempDir(),
		"some-vwc",
	)
	if err == nil {
		t.Fatal("provisionSelfSignedWebhookCert with empty namespace = nil error, want error")
	}
	if done != nil {
		t.Error("expected nil done channel on error")
	}
	if !strings.Contains(err.Error(), "namespace is required") {
		t.Errorf("error %q does not mention the missing namespace", err.Error())
	}
}
