// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"testing"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

// testPublicKeyPEM is a valid PEM-encoded ECDSA P-256 public key.
var testPublicKeyPEM = func() string {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		panic(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}()

func ivpFixture(spec stagesv1.ImageVerificationPolicySpec) *stagesv1.ImageVerificationPolicy {
	return &stagesv1.ImageVerificationPolicy{Spec: spec}
}

func keylessAuth(a stagesv1.KeylessAuthority) stagesv1.VerificationAuthority {
	return stagesv1.VerificationAuthority{Keyless: &a}
}

func TestValidateImageVerificationPolicy(t *testing.T) {
	good := stagesv1.KeylessAuthority{Issuer: "https://ci", Subject: "builder"}
	tests := []struct {
		name    string
		spec    stagesv1.ImageVerificationPolicySpec
		wantErr bool
	}{
		{
			name: "valid keyless policy",
			spec: stagesv1.ImageVerificationPolicySpec{Images: []string{"reg.io/**"}, Authorities: []stagesv1.VerificationAuthority{keylessAuth(good)}},
		},
		{
			name: "regexp identity is valid",
			spec: stagesv1.ImageVerificationPolicySpec{Images: []string{"reg.io/**"}, Authorities: []stagesv1.VerificationAuthority{keylessAuth(stagesv1.KeylessAuthority{IssuerRegExp: `^https://.*$`, SubjectRegExp: `^builder@.*$`})}},
		},
		{
			name:    "no images",
			spec:    stagesv1.ImageVerificationPolicySpec{Authorities: []stagesv1.VerificationAuthority{keylessAuth(good)}},
			wantErr: true,
		},
		{
			name:    "empty image glob",
			spec:    stagesv1.ImageVerificationPolicySpec{Images: []string{" "}, Authorities: []stagesv1.VerificationAuthority{keylessAuth(good)}},
			wantErr: true,
		},
		{
			name:    "no authorities",
			spec:    stagesv1.ImageVerificationPolicySpec{Images: []string{"reg.io/**"}},
			wantErr: true,
		},
		{
			name: "inline public key authority verifies",
			spec: stagesv1.ImageVerificationPolicySpec{Images: []string{"reg.io/**"}, Authorities: []stagesv1.VerificationAuthority{{Key: &stagesv1.KeyAuthority{PublicKey: testPublicKeyPEM}}}},
		},
		{
			name:    "malformed public key is rejected",
			spec:    stagesv1.ImageVerificationPolicySpec{Images: []string{"reg.io/**"}, Authorities: []stagesv1.VerificationAuthority{{Key: &stagesv1.KeyAuthority{PublicKey: "not a pem"}}}},
			wantErr: true,
		},
		{
			name:    "attestation requirement is rejected this version",
			spec:    stagesv1.ImageVerificationPolicySpec{Images: []string{"reg.io/**"}, Authorities: []stagesv1.VerificationAuthority{keylessAuth(good)}, RequireAttestations: []stagesv1.AttestationRequirement{{PredicateType: "https://slsa.dev/provenance/v1"}}},
			wantErr: true,
		},
		{
			name:    "both keyless and key set",
			spec:    stagesv1.ImageVerificationPolicySpec{Images: []string{"reg.io/**"}, Authorities: []stagesv1.VerificationAuthority{{Keyless: &good, Key: &stagesv1.KeyAuthority{PublicKey: testPublicKeyPEM}}}},
			wantErr: true,
		},
		{
			name:    "neither keyless nor key set",
			spec:    stagesv1.ImageVerificationPolicySpec{Images: []string{"reg.io/**"}, Authorities: []stagesv1.VerificationAuthority{{}}},
			wantErr: true,
		},
		{
			name:    "missing subject matcher",
			spec:    stagesv1.ImageVerificationPolicySpec{Images: []string{"reg.io/**"}, Authorities: []stagesv1.VerificationAuthority{keylessAuth(stagesv1.KeylessAuthority{Issuer: "https://ci"})}},
			wantErr: true,
		},
		{
			name:    "both issuer and issuerRegExp set",
			spec:    stagesv1.ImageVerificationPolicySpec{Images: []string{"reg.io/**"}, Authorities: []stagesv1.VerificationAuthority{keylessAuth(stagesv1.KeylessAuthority{Issuer: "https://ci", IssuerRegExp: "https://.*", Subject: "builder"})}},
			wantErr: true,
		},
		{
			name:    "invalid subject regexp",
			spec:    stagesv1.ImageVerificationPolicySpec{Images: []string{"reg.io/**"}, Authorities: []stagesv1.VerificationAuthority{keylessAuth(stagesv1.KeylessAuthority{Issuer: "https://ci", SubjectRegExp: "("})}},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateImageVerificationPolicy(ivpFixture(tt.spec))
			if tt.wantErr && err == nil {
				t.Fatal("want error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("want no error, got %v", err)
			}
		})
	}
}
