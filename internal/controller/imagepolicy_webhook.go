// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

// +kubebuilder:webhook:path=/validate-stages-metio-wtf-v1-imageverificationpolicy,mutating=false,failurePolicy=fail,sideEffects=None,groups=stages.metio.wtf,resources=imageverificationpolicies,verbs=create;update,versions=v1,name=vimageverificationpolicy.stages.metio.wtf,admissionReviewVersions=v1

// ImageVerificationPolicyValidator is the validating admission webhook for
// ImageVerificationPolicy. It rejects a policy that cannot be enforced — one with
// no accepted signer, a malformed keyless identity, or a capability this version
// does not yet verify — so a policy is never accepted that would silently hold every
// image it governs (or, worse, appear to verify one it cannot).
type ImageVerificationPolicyValidator struct{}

var _ admission.Validator[*stagesv1.ImageVerificationPolicy] = &ImageVerificationPolicyValidator{}

// ValidateCreate validates a new ImageVerificationPolicy.
func (v *ImageVerificationPolicyValidator) ValidateCreate(_ context.Context, p *stagesv1.ImageVerificationPolicy) (admission.Warnings, error) {
	return nil, ValidateImageVerificationPolicy(p)
}

// ValidateUpdate validates an updated ImageVerificationPolicy.
func (v *ImageVerificationPolicyValidator) ValidateUpdate(_ context.Context, _, newObj *stagesv1.ImageVerificationPolicy) (admission.Warnings, error) {
	if !newObj.GetDeletionTimestamp().IsZero() {
		return nil, nil
	}
	return nil, ValidateImageVerificationPolicy(newObj)
}

// ValidateDelete is a no-op; removing a policy is always allowed.
func (v *ImageVerificationPolicyValidator) ValidateDelete(_ context.Context, _ *stagesv1.ImageVerificationPolicy) (admission.Warnings, error) {
	return nil, nil
}

// SetupWebhookWithManager registers the validating webhook on the manager's webhook
// server.
func (v *ImageVerificationPolicyValidator) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &stagesv1.ImageVerificationPolicy{}).
		WithValidator(v).
		Complete()
}

// ValidateImageVerificationPolicy is the single source of truth for policy
// validation. A policy must govern at least one image glob and carry at least one
// authority that this version can verify — a keyless identity with a well-formed
// issuer and subject matcher.
func ValidateImageVerificationPolicy(p *stagesv1.ImageVerificationPolicy) error {
	spec := p.Spec
	if len(spec.Images) == 0 {
		return fmt.Errorf("spec.images must list at least one glob")
	}
	for i, g := range spec.Images {
		if strings.TrimSpace(g) == "" {
			return fmt.Errorf("spec.images[%d] must not be empty", i)
		}
	}
	for i, g := range spec.Skip {
		if strings.TrimSpace(g) == "" {
			return fmt.Errorf("spec.skip[%d] must not be empty", i)
		}
	}

	// Attestation requirements are declared in the API but not yet verified; accepting
	// one would leave a documented guarantee silently unenforced.
	if len(spec.RequireAttestations) > 0 {
		return fmt.Errorf("spec.requireAttestations is not enforced in this version; remove it")
	}

	if len(spec.Authorities) == 0 {
		return fmt.Errorf("spec.authorities must list at least one keyless authority")
	}
	for i := range spec.Authorities {
		if err := validateAuthority(i, &spec.Authorities[i]); err != nil {
			return err
		}
	}
	return nil
}

func validateAuthority(i int, a *stagesv1.VerificationAuthority) error {
	switch {
	case a.Keyless != nil && a.Key != nil:
		return fmt.Errorf("spec.authorities[%d] sets both keyless and key; set exactly one", i)
	case a.Keyless == nil && a.Key == nil:
		return fmt.Errorf("spec.authorities[%d] sets neither keyless nor key; set exactly one", i)
	case a.Key != nil:
		return fmt.Errorf("spec.authorities[%d] is a key authority, which is not supported in this version; use keyless", i)
	}

	k := a.Keyless
	if err := validateMatcher(i, "issuer", k.Issuer, k.IssuerRegExp); err != nil {
		return err
	}
	if err := validateMatcher(i, "subject", k.Subject, k.SubjectRegExp); err != nil {
		return err
	}
	return nil
}

// validateMatcher enforces that exactly one of the exact/regexp form is set for an
// identity field, and that a regexp compiles — the same shape sigstore-go's
// NewShortCertificateIdentity accepts, checked at admission so a bad identity is
// rejected on write instead of holding every governed image at reconcile.
func validateMatcher(i int, field, exact, pattern string) error {
	switch {
	case exact == "" && pattern == "":
		return fmt.Errorf("spec.authorities[%d].keyless must set %s or %sRegExp", i, field, field)
	case exact != "" && pattern != "":
		return fmt.Errorf("spec.authorities[%d].keyless sets both %s and %sRegExp; set exactly one", i, field, field)
	}
	if pattern != "" {
		if _, err := regexp.Compile(pattern); err != nil {
			return fmt.Errorf("spec.authorities[%d].keyless.%sRegExp is not a valid regexp: %w", i, field, err)
		}
	}
	return nil
}
