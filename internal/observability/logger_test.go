// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package observability

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestNewLogger(t *testing.T) {
	tests := []struct {
		name      string
		level     string
		format    string
		logAt     string // level to emit at
		message   string
		wantOut   bool // whether the message should appear
		wantJSON  bool // output should parse as JSON
		wantPlain bool // output should be text (key=value), not JSON
	}{
		{name: "json info default", level: "info", format: "json", logAt: "info", message: "hello", wantOut: true, wantJSON: true},
		{name: "text format selected", level: "info", format: "text", logAt: "info", message: "hello", wantOut: true, wantPlain: true},
		{name: "unknown format falls back to json", level: "info", format: "weird", logAt: "info", message: "hello", wantOut: true, wantJSON: true},
		{name: "empty format falls back to json", level: "info", format: "", logAt: "info", message: "hello", wantOut: true, wantJSON: true},
		{name: "debug level emits debug", level: "debug", format: "json", logAt: "debug", message: "dbg", wantOut: true, wantJSON: true},
		{name: "info level suppresses debug", level: "info", format: "json", logAt: "debug", message: "dbg", wantOut: false},
		{name: "warn level suppresses info", level: "warn", format: "json", logAt: "info", message: "inf", wantOut: false},
		{name: "warn level emits warn", level: "warn", format: "json", logAt: "warn", message: "wrn", wantOut: true, wantJSON: true},
		{name: "error level suppresses warn", level: "error", format: "json", logAt: "warn", message: "wrn", wantOut: false},
		{name: "error level emits error", level: "error", format: "json", logAt: "error", message: "err", wantOut: true, wantJSON: true},
		{name: "unknown level defaults to info, emits info", level: "loud", format: "json", logAt: "info", message: "inf", wantOut: true, wantJSON: true},
		{name: "unknown level defaults to info, suppresses debug", level: "loud", format: "json", logAt: "debug", message: "dbg", wantOut: false},
		{name: "level is case-insensitive", level: "DEBUG", format: "json", logAt: "debug", message: "dbg", wantOut: true, wantJSON: true},
		{name: "format is case-insensitive", level: "info", format: "TEXT", logAt: "info", message: "hello", wantOut: true, wantPlain: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := NewLogger(&buf, tc.level, tc.format)

			switch tc.logAt {
			case "debug":
				logger.Debug(tc.message)
			case "info":
				logger.Info(tc.message)
			case "warn":
				logger.Warn(tc.message)
			case "error":
				logger.Error(tc.message)
			default:
				t.Fatalf("unhandled logAt %q", tc.logAt)
			}

			out := buf.String()
			if tc.wantOut && out == "" {
				t.Fatalf("expected output for %q at %q, got none", tc.message, tc.logAt)
			}
			if !tc.wantOut {
				if out != "" {
					t.Fatalf("expected no output (level filtered), got %q", out)
				}
				return
			}
			if !strings.Contains(out, tc.message) {
				t.Fatalf("output %q does not contain message %q", out, tc.message)
			}
			if tc.wantJSON {
				var m map[string]any
				if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &m); err != nil {
					t.Fatalf("expected JSON output, parse failed: %v (out=%q)", err, out)
				}
			}
			if tc.wantPlain {
				if json.Valid([]byte(strings.TrimSpace(out))) {
					t.Fatalf("expected text output, but it parsed as JSON: %q", out)
				}
				if !strings.Contains(out, "msg=") && !strings.Contains(out, "msg=\"") {
					t.Fatalf("expected slog text output with msg= key, got %q", out)
				}
			}
		})
	}
}
