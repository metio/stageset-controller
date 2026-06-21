// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package artifact

import (
	"bytes"
	"errors"
	"io"
	"net"
	"testing"
)

// A decompressed stream sized exactly at the cap is valid — only a byte past it
// is a bomb. The reader used to reject the exactly-at-cap case.
func TestCappedReader_ExactCapSucceeds(t *testing.T) {
	data := bytes.Repeat([]byte("x"), 100)
	cr := &cappedReader{r: bytes.NewReader(data), remaining: 100}
	got, err := io.ReadAll(cr)
	if err != nil {
		t.Fatalf("stream exactly at the cap was rejected: %v", err)
	}
	if len(got) != 100 {
		t.Fatalf("read %d bytes, want 100", len(got))
	}
}

func TestCappedReader_OverCapFails(t *testing.T) {
	data := bytes.Repeat([]byte("x"), 101)
	cr := &cappedReader{r: bytes.NewReader(data), remaining: 100}
	if _, err := io.ReadAll(cr); !errors.Is(err, errDecompressedCapped) {
		t.Fatalf("over-cap stream err = %v, want errDecompressedCapped", err)
	}
}

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
		{"inet_aton single-int loopback", "http://2130706433/x", ErrForbiddenHost},
		{"inet_aton hex loopback", "http://0x7f000001/x", ErrForbiddenHost},
		{"inet_aton octal loopback", "http://017700000001/x", ErrForbiddenHost},
		{"inet_aton short-dotted loopback", "http://127.1/x", ErrForbiddenHost},
		{"inet_aton hex public ok", "https://0x08080808/x", nil},        // 8.8.8.8, permitted
		{"inet_aton single-int rfc1918 ok", "http://3232235521/x", nil}, // 192.168.0.1, permitted
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

func TestParseInetAtonIPv4(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want string // empty == nil (not an IP literal)
	}{
		{"2130706433", "127.0.0.1"},
		{"0x7f000001", "127.0.0.1"},
		{"017700000001", "127.0.0.1"},
		{"127.1", "127.0.0.1"},
		{"127.0.1", "127.0.0.1"},
		{"0x08080808", "8.8.8.8"},
		{"3232235521", "192.168.0.1"},
		{"4294967296", ""},             // overflows 32 bits
		{"1.2.3.4.5", ""},              // too many parts
		{"256.0.0.1", ""},              // leading part out of byte range
		{"127.0.0.0x100", ""},          // final part out of range for one byte
		{"not-a-number", ""},           // non-numeric
		{"", ""},                       // empty
		{"source.flux-system.svc", ""}, // a real hostname is not an IP literal
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got := parseInetAtonIPv4(tc.in)
			if tc.want == "" {
				if got != nil {
					t.Fatalf("parseInetAtonIPv4(%q) = %v, want nil", tc.in, got)
				}
				return
			}
			if got == nil || got.String() != tc.want {
				t.Fatalf("parseInetAtonIPv4(%q) = %v, want %s", tc.in, got, tc.want)
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
