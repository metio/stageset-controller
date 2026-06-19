// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package actions

import (
	"net/url"
	"path"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// TestAllowedURL_GlobMatching tables the host allow-list glob matcher. The
// matcher is path.Match, in which "/" is the only separator-significant byte —
// "." is an ordinary byte, so "*" matches across dots (a whole multi-label
// host), while "*.slack.com" still requires a leading label plus the literal
// ".slack.com". It also pins that any non-empty allow-list rejects a host none
// of its patterns match.
func TestAllowedURL_GlobMatching(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		hosts []string
		host  string
		ok    bool
	}{
		{"exact match", []string{"hooks.slack.com"}, "hooks.slack.com", true},
		{"exact mismatch", []string{"hooks.slack.com"}, "evil.example", false},
		{"subdomain glob matches", []string{"*.slack.com"}, "hooks.slack.com", true},
		{"subdomain glob needs a label", []string{"*.slack.com"}, "slack.com", false},
		{"bare star matches single label", []string{"*"}, "intranet", true},
		{"bare star crosses dots", []string{"*"}, "a.b.c", true},
		{"any of several", []string{"a.example", "*.slack.com"}, "hooks.slack.com", true},
		{"none of several", []string{"a.example", "*.slack.com"}, "b.example", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := &Executor{AllowedHosts: tc.hosts}
			err := e.allowedURL("https://" + tc.host + "/x")
			if (err == nil) != tc.ok {
				t.Fatalf("allowedURL host=%q hosts=%v err=%v, want ok=%v", tc.host, tc.hosts, err, tc.ok)
			}
		})
	}
}

// TestAllowedURL_Property pins the allow-list contract against generated hosts
// and patterns: with a non-empty allow-list, allowedURL accepts a host iff some
// pattern path.Match-es it. The matcher's own semantics are the oracle, so the
// property verifies allowedURL faithfully delegates to it (and short-circuits on
// the first matching pattern) for arbitrary inputs.
func TestAllowedURL_Property(t *testing.T) {
	t.Parallel()
	// Hosts and patterns drawn from a small alphabet so collisions (matches) are
	// common enough to exercise both branches; "*" and "." are included so globs
	// and label boundaries arise.
	label := rapid.StringOfN(rapid.RuneFrom([]rune("ab*")), 1, 5, -1)
	rapid.Check(t, func(rt *rapid.T) {
		// Build a dotted host from labels so "." appears at label boundaries but
		// the host never starts/ends with a dot (those parse to a different host).
		parts := rapid.SliceOfN(label, 1, 3).Draw(rt, "labels")
		host := strings.Join(parts, ".")
		patterns := rapid.SliceOfN(label, 1, 4).Draw(rt, "patterns")

		// The matcher sees the URL hostname with a trailing dot trimmed; mirror
		// that so the oracle compares against the same string allowedURL does.
		u, err := url.Parse("https://" + host + "/x")
		if err != nil {
			return
		}
		matchHost := strings.TrimSuffix(u.Hostname(), ".")
		if matchHost == "" {
			return
		}

		want := false
		for _, p := range patterns {
			if ok, merr := path.Match(p, matchHost); merr == nil && ok {
				want = true
				break
			}
		}

		e := &Executor{AllowedHosts: patterns}
		got := e.allowedURL("https://"+host+"/x") == nil
		if got != want {
			rt.Fatalf("allowedURL(host=%q matchHost=%q patterns=%v) accepted=%v, want %v", host, matchHost, patterns, got, want)
		}
	})
}

// TestAllowedURL_EmptyAllowlist_Property pins the empty-allow-list policy:
// allow-all minus loopback/link-local/multicast/unspecified literals and
// "localhost". A generated public-looking hostname is always accepted; the
// known-forbidden literals are always rejected.
func TestAllowedURL_EmptyAllowlist_Property(t *testing.T) {
	t.Parallel()
	forbidden := []string{
		"127.0.0.1", "localhost", "169.254.169.254", "0.0.0.0",
		"[::1]", "224.0.0.1",
	}
	for _, h := range forbidden {
		e := &Executor{}
		if err := e.allowedURL("https://" + h + "/x"); err == nil {
			t.Fatalf("empty allow-list must still reject %q", h)
		}
	}

	// A generated DNS-name-shaped host (no IP literal, not "localhost") is allowed
	// when no allow-list is configured.
	name := rapid.StringOfN(rapid.RuneFrom([]rune("abcdefghijklmnopqrstuvwxyz")), 1, 10, -1)
	rapid.Check(t, func(rt *rapid.T) {
		host := name.Draw(rt, "host") + ".example.com"
		e := &Executor{}
		if err := e.allowedURL("https://" + host + "/x"); err != nil {
			rt.Fatalf("empty allow-list should allow public host %q, got %v", host, err)
		}
	})
}
