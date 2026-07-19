// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package sigstore

import (
	"errors"
	"fmt"

	"github.com/sigstore/sigstore-go/pkg/verify"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

// verifyImage reports nil when at least one of the policy's keyless authorities
// verifies at least one of the image's bundles against its resolved digest — the
// "any authority, any bundle" semantics the policy documents. verifier carries the
// trusted material (the production Sigstore root, or a VirtualSigstore in tests), so
// this function holds the whole signature-checking decision and is unit-testable
// without a registry.
//
// digestAlgo/digestHex are the raw (not hex-string) bytes of the image manifest's
// digest — cosign signs over that digest, and WithArtifactDigest pins the bundle's
// signed payload to it, so a bundle lifted from another image can't satisfy the gate.
func verifyImage(verifier *verify.Verifier, entities []verify.SignedEntity, digestAlgo string, digestHex []byte, authorities []stagesv1.VerificationAuthority) error {
	var errs []error
	sawKeyless := false
	for i := range authorities {
		keyless := authorities[i].Keyless
		if keyless == nil {
			// Key authorities are rejected at admission in this version; a policy
			// that somehow carries one contributes no accepted signer.
			continue
		}
		sawKeyless = true
		certID, err := verify.NewShortCertificateIdentity(keyless.Issuer, keyless.IssuerRegExp, keyless.Subject, keyless.SubjectRegExp)
		if err != nil {
			errs = append(errs, fmt.Errorf("build certificate identity: %w", err))
			continue
		}
		policy := verify.NewPolicy(
			verify.WithArtifactDigest(digestAlgo, digestHex),
			verify.WithCertificateIdentity(certID),
		)
		for _, entity := range entities {
			if _, verr := verifier.Verify(entity, policy); verr != nil {
				errs = append(errs, verr)
				continue
			}
			return nil // this authority verified a bundle: the image is trusted.
		}
	}
	if !sawKeyless {
		return errors.New("policy lists no keyless authority to verify against")
	}
	if len(entities) == 0 {
		return errors.New("no Sigstore bundle is attached to the image")
	}
	return fmt.Errorf("no policy authority verified any attached bundle: %w", errors.Join(errs...))
}
