// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package sigstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"testing"

	"github.com/sigstore/sigstore-go/pkg/testing/ca"
	"github.com/sigstore/sigstore-go/pkg/verify"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

const (
	testIdentity = "https://ci.example/.github/workflows/release.yml@refs/heads/main"
	testIssuer   = "https://token.actions.githubusercontent.com"
)

// signedFixture mints a VirtualSigstore, signs a fake image manifest, and returns the
// keyless verifier plus the signed entity and the digest cosign would have signed —
// everything verifyImage needs, with no registry or network.
func signedFixture(t *testing.T) (*verify.Verifier, verify.SignedEntity, []byte) {
	t.Helper()
	vs, err := ca.NewVirtualSigstore()
	if err != nil {
		t.Fatalf("NewVirtualSigstore: %v", err)
	}
	manifest := []byte("fake image manifest bytes")
	sum := sha256.Sum256(manifest)
	entity, err := vs.Sign(testIdentity, testIssuer, manifest)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	verifier, err := verify.NewVerifier(vs, verify.WithTransparencyLog(1), verify.WithObserverTimestamps(1))
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	return verifier, entity, sum[:]
}

func keyless(issuer, issuerRE, subject, subjectRE string) stagesv1.VerificationAuthority {
	return stagesv1.VerificationAuthority{Keyless: &stagesv1.KeylessAuthority{
		Issuer: issuer, IssuerRegExp: issuerRE, Subject: subject, SubjectRegExp: subjectRE,
	}}
}

func TestVerifyImage(t *testing.T) {
	tests := []struct {
		name        string
		authorities []stagesv1.VerificationAuthority
		tamper      bool // verify against a different digest than was signed
		noEntities  bool
		wantErr     bool
	}{
		{
			name:        "exact issuer and subject verify",
			authorities: []stagesv1.VerificationAuthority{keyless(testIssuer, "", testIdentity, "")},
		},
		{
			name:        "subject regexp verifies",
			authorities: []stagesv1.VerificationAuthority{keyless(testIssuer, "", "", `^https://ci\.example/.*@refs/heads/main$`)},
		},
		{
			name:        "issuer regexp verifies",
			authorities: []stagesv1.VerificationAuthority{keyless("", `^https://token\.actions\..*$`, testIdentity, "")},
		},
		{
			name:        "second authority verifies when the first does not",
			authorities: []stagesv1.VerificationAuthority{keyless(testIssuer, "", "someone-else", ""), keyless(testIssuer, "", testIdentity, "")},
			wantErr:     false,
		},
		{
			name:        "wrong subject is rejected",
			authorities: []stagesv1.VerificationAuthority{keyless(testIssuer, "", "not-the-builder", "")},
			wantErr:     true,
		},
		{
			name:        "wrong issuer is rejected",
			authorities: []stagesv1.VerificationAuthority{keyless("https://accounts.google.com", "", testIdentity, "")},
			wantErr:     true,
		},
		{
			name:        "a different digest is rejected",
			authorities: []stagesv1.VerificationAuthority{keyless(testIssuer, "", testIdentity, "")},
			tamper:      true,
			wantErr:     true,
		},
		{
			name:        "a policy with no keyless authority is rejected",
			authorities: []stagesv1.VerificationAuthority{{Key: &stagesv1.KeyAuthority{}}},
			wantErr:     true,
		},
		{
			name:        "no attached bundle is rejected",
			authorities: []stagesv1.VerificationAuthority{keyless(testIssuer, "", testIdentity, "")},
			noEntities:  true,
			wantErr:     true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			verifier, entity, digest := signedFixture(t)
			entities := []verify.SignedEntity{entity}
			if tt.noEntities {
				entities = nil
			}
			if tt.tamper {
				other := sha256.Sum256([]byte("a different manifest"))
				digest = other[:]
			}
			err := verifyImage(verifier, entities, "sha256", digest, tt.authorities)
			if tt.wantErr && err == nil {
				t.Fatal("want a verification error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("want success, got %v", err)
			}
		})
	}
}

type errLayer struct {
	data []byte
	err  error
}

func (l errLayer) Uncompressed() (io.ReadCloser, error) {
	if l.err != nil {
		return nil, l.err
	}
	return io.NopCloser(bytes.NewReader(l.data)), nil
}

func TestParseBundleLayer_RejectsBadInput(t *testing.T) {
	if _, err := parseBundleLayer(errLayer{err: errors.New("boom")}); err == nil {
		t.Fatal("want error when the layer is unreadable")
	}
	if _, err := parseBundleLayer(errLayer{data: []byte("not json")}); err == nil {
		t.Fatal("want error when the layer is not a bundle")
	}
	if _, err := parseBundleLayer(errLayer{data: []byte(`{"mediaType":"application/vnd.dev.sigstore.bundle.v0.3+json"}`)}); err == nil {
		t.Fatal("want error when the bundle has no verification material")
	}
}

// TestVerify_RejectsUnsupportedConfig pins the fail-closed guards for config this
// version does not yet enforce: a key authority and any attestation requirement are
// refused before any network call, so nothing unverified slips through.
func TestVerify_RejectsUnsupportedConfig(t *testing.T) {
	v := New()
	if _, err := v.Verify(context.Background(), "reg.io/app:1", []stagesv1.VerificationAuthority{{Key: &stagesv1.KeyAuthority{}}}, nil); err == nil {
		t.Fatal("want a key-authority refusal")
	}
	if _, err := v.Verify(context.Background(), "reg.io/app:1", nil, []stagesv1.AttestationRequirement{{PredicateType: "x"}}); err == nil {
		t.Fatal("want an attestation-requirement refusal")
	}
}
