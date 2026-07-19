// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package sigstore

import (
	"errors"
	"fmt"
	"time"

	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/verify"
	"github.com/sigstore/sigstore/pkg/cryptoutils"
	sigsig "github.com/sigstore/sigstore/pkg/signature"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

// verifyImage reports nil when at least one of the policy's authorities verifies at
// least one of the image's bundles against its resolved digest — the "any authority,
// any bundle" semantics the policy documents. keylessVerifier is a lazy provider for
// the Sigstore-root-backed verifier (built only if a keyless authority is present, so
// a key-only policy never triggers a trusted-root fetch). This function holds the
// whole signature-checking decision and is unit-testable without a registry.
//
// digestAlgo/digestHex are the raw (not hex-string) bytes of the image manifest's
// digest — cosign signs over that digest, and WithArtifactDigest pins the bundle's
// signed payload to it, so a bundle lifted from another image can't satisfy the gate.
func verifyImage(keylessVerifier func() (*verify.Verifier, error), entities []verify.SignedEntity, digestAlgo string, digestHex []byte, authorities []stagesv1.VerificationAuthority) error {
	var errs []error
	accepted := false
	for i := range authorities {
		a := &authorities[i]
		switch {
		case a.Keyless != nil:
			accepted = true
			verifier, err := keylessVerifier()
			if err != nil {
				return err
			}
			if err := verifyKeyless(verifier, a.Keyless, entities, digestAlgo, digestHex); err != nil {
				errs = append(errs, err)
				continue
			}
			return nil
		case a.Key != nil:
			accepted = true
			if err := verifyKey(a.Key, entities, digestAlgo, digestHex); err != nil {
				errs = append(errs, err)
				continue
			}
			return nil
		}
	}
	if !accepted {
		return errors.New("policy lists no authority to verify against")
	}
	if len(entities) == 0 {
		return errors.New("no Sigstore bundle is attached to the image")
	}
	return fmt.Errorf("no policy authority verified any attached bundle: %w", errors.Join(errs...))
}

// verifyKeyless checks the bundles against a Fulcio certificate identity.
func verifyKeyless(verifier *verify.Verifier, keyless *stagesv1.KeylessAuthority, entities []verify.SignedEntity, digestAlgo string, digestHex []byte) error {
	certID, err := verify.NewShortCertificateIdentity(keyless.Issuer, keyless.IssuerRegExp, keyless.Subject, keyless.SubjectRegExp)
	if err != nil {
		return fmt.Errorf("build certificate identity: %w", err)
	}
	policy := verify.NewPolicy(
		verify.WithArtifactDigest(digestAlgo, digestHex),
		verify.WithCertificateIdentity(certID),
	)
	return verifyEntities(verifier, entities, policy)
}

// verifyKey checks the bundles against an inline cosign public key. The key is
// long-lived (no certificate expiry), so verification uses the current time and the
// key material alone — matching `cosign verify --key`: the signature over the digest
// is what establishes trust, independent of a transparency-log entry.
func verifyKey(key *stagesv1.KeyAuthority, entities []verify.SignedEntity, digestAlgo string, digestHex []byte) error {
	pub, err := cryptoutils.UnmarshalPEMToPublicKey([]byte(key.PublicKey))
	if err != nil {
		return fmt.Errorf("parse public key: %w", err)
	}
	sv, err := sigsig.LoadDefaultVerifier(pub)
	if err != nil {
		return fmt.Errorf("load public key verifier: %w", err)
	}
	keyMaterial := root.NewTrustedPublicKeyMaterial(func(string) (root.TimeConstrainedVerifier, error) {
		return root.NewExpiringKey(sv, time.Time{}, time.Time{}), nil
	})
	verifier, err := verify.NewVerifier(keyMaterial, verify.WithCurrentTime())
	if err != nil {
		return fmt.Errorf("build key verifier: %w", err)
	}
	policy := verify.NewPolicy(
		verify.WithArtifactDigest(digestAlgo, digestHex),
		verify.WithKey(),
	)
	return verifyEntities(verifier, entities, policy)
}

// verifyEntities returns nil if any entity satisfies the policy.
func verifyEntities(verifier *verify.Verifier, entities []verify.SignedEntity, policy verify.PolicyBuilder) error {
	var errs []error
	for _, entity := range entities {
		if _, err := verifier.Verify(entity, policy); err != nil {
			errs = append(errs, err)
			continue
		}
		return nil
	}
	if len(errs) == 0 {
		return errors.New("no Sigstore bundle is attached to the image")
	}
	return errors.Join(errs...)
}
