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

	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/sign"
	"github.com/sigstore/sigstore-go/pkg/testing/ca"
	"github.com/sigstore/sigstore-go/pkg/verify"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

const (
	testIdentity = "https://ci.example/.github/workflows/release.yml@refs/heads/main"
	testIssuer   = "https://token.actions.githubusercontent.com"
)

var manifest = []byte("fake image manifest bytes")

// keylessFixture mints a VirtualSigstore, signs a fake image manifest, and returns a
// lazy provider for the keyless verifier plus the signed entity and the digest cosign
// would have signed — everything verifyImage needs, with no registry or network.
func keylessFixture(t *testing.T) (func() (*verify.Verifier, error), verify.SignedEntity, []byte) {
	t.Helper()
	vs, err := ca.NewVirtualSigstore()
	if err != nil {
		t.Fatalf("NewVirtualSigstore: %v", err)
	}
	sum := sha256.Sum256(manifest)
	entity, err := vs.Sign(testIdentity, testIssuer, manifest)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	verifier, err := verify.NewVerifier(vs, verify.WithTransparencyLog(1), verify.WithObserverTimestamps(1))
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	return func() (*verify.Verifier, error) { return verifier, nil }, entity, sum[:]
}

func keyless(issuer, issuerRE, subject, subjectRE string) stagesv1.VerificationAuthority {
	return stagesv1.VerificationAuthority{Keyless: &stagesv1.KeylessAuthority{
		Issuer: issuer, IssuerRegExp: issuerRE, Subject: subject, SubjectRegExp: subjectRE,
	}}
}

// noKeyless is a provider that fails if called — a key-only policy must never build
// the Sigstore-root-backed keyless verifier.
func noKeyless() (*verify.Verifier, error) {
	return nil, errors.New("keyless verifier must not be built")
}

func TestVerifyImage_Keyless(t *testing.T) {
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
			name:        "a policy with no authority is rejected",
			authorities: []stagesv1.VerificationAuthority{{}},
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
			provider, entity, digest := keylessFixture(t)
			entities := []verify.SignedEntity{entity}
			if tt.noEntities {
				entities = nil
			}
			if tt.tamper {
				other := sha256.Sum256([]byte("a different manifest"))
				digest = other[:]
			}
			err := verifyImage(provider, entities, "sha256", digest, tt.authorities)
			if tt.wantErr && err == nil {
				t.Fatal("want a verification error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("want success, got %v", err)
			}
		})
	}
}

// keySignedFixture signs the fake manifest with a fresh ephemeral key and returns the
// bundle, the key's PEM, and the signed digest.
func keySignedFixture(t *testing.T) (verify.SignedEntity, string, []byte) {
	t.Helper()
	kp, err := sign.NewEphemeralKeypair(nil)
	if err != nil {
		t.Fatalf("NewEphemeralKeypair: %v", err)
	}
	pubPEM, err := kp.GetPublicKeyPem()
	if err != nil {
		t.Fatalf("GetPublicKeyPem: %v", err)
	}
	pb, err := sign.Bundle(&sign.PlainData{Data: manifest}, kp, sign.BundleOptions{})
	if err != nil {
		t.Fatalf("sign.Bundle: %v", err)
	}
	sum := sha256.Sum256(manifest)
	return &bundle.Bundle{Bundle: pb}, pubPEM, sum[:]
}

func TestVerifyImage_Key(t *testing.T) {
	entity, pubPEM, digest := keySignedFixture(t)
	keyAuth := func(pem string) []stagesv1.VerificationAuthority {
		return []stagesv1.VerificationAuthority{{Key: &stagesv1.KeyAuthority{PublicKey: pem}}}
	}

	t.Run("matching key verifies", func(t *testing.T) {
		if err := verifyImage(noKeyless, []verify.SignedEntity{entity}, "sha256", digest, keyAuth(pubPEM)); err != nil {
			t.Fatalf("want success, got %v", err)
		}
	})

	t.Run("a different key is rejected", func(t *testing.T) {
		_, otherPEM, _ := keySignedFixture(t)
		if err := verifyImage(noKeyless, []verify.SignedEntity{entity}, "sha256", digest, keyAuth(otherPEM)); err == nil {
			t.Fatal("want rejection when the key does not match")
		}
	})

	t.Run("a different digest is rejected", func(t *testing.T) {
		other := sha256.Sum256([]byte("a different manifest"))
		if err := verifyImage(noKeyless, []verify.SignedEntity{entity}, "sha256", other[:], keyAuth(pubPEM)); err == nil {
			t.Fatal("want rejection when the digest does not match")
		}
	})

	t.Run("a malformed key is rejected", func(t *testing.T) {
		if err := verifyImage(noKeyless, []verify.SignedEntity{entity}, "sha256", digest, keyAuth("not a pem key")); err == nil {
			t.Fatal("want rejection for a malformed key")
		}
	})
}

func TestParseReference_Insecure(t *testing.T) {
	v := New(WithInsecureRegistries([]string{"registry.internal:5000"}))
	insecure, err := v.parseReference("registry.internal:5000/app:v1")
	if err != nil {
		t.Fatalf("parse insecure ref: %v", err)
	}
	if got := insecure.Context().Registry.Scheme(); got != "http" {
		t.Fatalf("insecure registry scheme = %q, want http", got)
	}

	secure, err := v.parseReference("ghcr.io/acme/app:v1")
	if err != nil {
		t.Fatalf("parse secure ref: %v", err)
	}
	if got := secure.Context().Registry.Scheme(); got != "https" {
		t.Fatalf("unlisted registry scheme = %q, want https", got)
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

// TestVerify_RejectsUnsupportedConfig pins the fail-closed guard for config this
// version does not yet enforce: an attestation requirement is refused before any
// network call, so nothing unverified slips through.
func TestVerify_RejectsUnsupportedConfig(t *testing.T) {
	v := New()
	if _, err := v.Verify(context.Background(), "reg.io/app:1", nil, []stagesv1.AttestationRequirement{{PredicateType: "x"}}); err == nil {
		t.Fatal("want an attestation-requirement refusal")
	}
}
