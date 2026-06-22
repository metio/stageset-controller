// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package rollbackstore

import (
	"bytes"
	"strings"
	"testing"
)

func TestValidDigest(t *testing.T) {
	valid := []string{
		"sha256:abc123",
		"sha512:DEADBEEF",
		"sha256:" + strings.Repeat("a", 64),
	}
	for _, d := range valid {
		if err := ValidDigest(d); err != nil {
			t.Errorf("ValidDigest(%q) = %v, want nil", d, err)
		}
	}
	// A path-bearing or malformed digest must be rejected so it cannot escape
	// the rollback-store key prefix on the S3 backend.
	invalid := []string{
		"",
		"noseparator",
		"sha256:",
		":abc",
		"sha256:../../etc/passwd",
		"sha256:ab/cd",
		"../sha256:abcd",
		"sha 256:abcd",
		"sha256:xyz", // non-hex
	}
	for _, d := range invalid {
		if err := ValidDigest(d); err == nil {
			t.Errorf("ValidDigest(%q) = nil, want error", d)
		}
	}
}

func TestReadCapped(t *testing.T) {
	// At the cap is allowed; one byte over is rejected.
	atCap := bytes.Repeat([]byte("x"), 16)
	if got, err := readCapped(bytes.NewReader(atCap)); err != nil || len(got) != 16 {
		t.Fatalf("readCapped at small size = (%d bytes, %v), want (16, nil)", len(got), err)
	}
	over := make([]byte, MaxObjectBytes+1)
	if _, err := readCapped(bytes.NewReader(over)); err == nil {
		t.Fatal("readCapped over MaxObjectBytes = nil, want error")
	}
}
