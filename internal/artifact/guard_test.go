// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package artifact

import (
	"errors"
	"net"
	"testing"
)

func TestValidateHTTPURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		url     string
		wantErr error
	}{
		{"https ok", "https://source.flux-system.svc/ns/a/rev.tar.gz", nil},
		{"http private ok", "http://10.1.2.3:8082/ns/a/rev.tar.gz", nil},
		{"loopback literal", "http://127.0.0.1/x", ErrForbiddenHost},
		{"localhost", "http://localhost/x", ErrForbiddenHost},
		{"link-local metadata", "http://169.254.169.254/latest", ErrForbiddenHost},
		{"unspecified", "http://0.0.0.0/x", ErrForbiddenHost},
		{"bad scheme", "file:///etc/passwd", ErrInvalidScheme},
		{"no host", "http://", ErrMissingHost},
		{"trailing-dot loopback", "http://127.0.0.1./x", ErrForbiddenHost},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateHTTPURL(tc.url)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("validateHTTPURL(%q) = %v, want nil", tc.url, err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("validateHTTPURL(%q) = %v, want %v", tc.url, err, tc.wantErr)
			}
		})
	}
}

func TestForbiddenIP(t *testing.T) {
	t.Parallel()
	if forbiddenIP(net.ParseIP("10.0.0.1")) != nil {
		t.Fatal("RFC1918 must be permitted (in-cluster source servers live there)")
	}
	if forbiddenIP(net.ParseIP("127.0.0.1")) == nil {
		t.Fatal("loopback must be rejected")
	}
	if forbiddenIP(net.ParseIP("169.254.169.254")) == nil {
		t.Fatal("link-local metadata must be rejected")
	}
}

func TestNormaliseEntry(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, raw, prefix, want string
		ok                      bool
	}{
		{"plain", "a/b.yaml", "", "a/b.yaml", true},
		{"prefix strip", "manifests/a.yaml", "manifests", "a.yaml", true},
		{"out of prefix", "other/a.yaml", "manifests", "", false},
		{"traversal", "../escape.yaml", "", "", false},
		{"absolute", "/etc/passwd", "", "", false},
		{"backslash", "a\\b.yaml", "", "", false},
		{"dotfile", ".hidden/x.yaml", "", "", false},
		{"bad byte", "a/b c.yaml", "", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := normaliseEntry(tc.raw, tc.prefix)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("normaliseEntry(%q,%q) = (%q,%v), want (%q,%v)", tc.raw, tc.prefix, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestVerifyDigest(t *testing.T) {
	t.Parallel()
	const got = "0000000000000000000000000000000000000000000000000000000000000000"
	if err := verifyDigest(got, "sha256:"+got); err != nil {
		t.Fatalf("matching digest: %v", err)
	}
	if err := verifyDigest(got, "sha256:dead"); !errors.Is(err, ErrDigestInvalid) {
		t.Fatalf("short hex: want ErrDigestInvalid, got %v", err)
	}
	if err := verifyDigest(got, "md5:"+got); !errors.Is(err, ErrDigestInvalid) {
		t.Fatalf("wrong algo: want ErrDigestInvalid, got %v", err)
	}
	other := "1111111111111111111111111111111111111111111111111111111111111111"
	if err := verifyDigest(got, "sha256:"+other); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("mismatch: want ErrDigestMismatch, got %v", err)
	}
}

func TestGroupOf(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"jaas.metio.wtf/v1":           "jaas.metio.wtf",
		"source.toolkit.fluxcd.io/v1": "source.toolkit.fluxcd.io",
		"v1":                          "",
	}
	for in, want := range cases {
		if got := groupOf(in); got != want {
			t.Fatalf("groupOf(%q) = %q, want %q", in, got, want)
		}
	}
}
