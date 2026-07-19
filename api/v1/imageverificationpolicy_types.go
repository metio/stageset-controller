// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ImageVerificationPolicySpec declares how the images matched by its globs must be
// verified before a StageSet stage applies them.
type ImageVerificationPolicySpec struct {
	// Images are glob patterns (path.Match against the image ref without its tag or
	// digest). This policy governs every image whose ref matches any of them.
	// +required
	// +kubebuilder:validation:MinItems=1
	Images []string `json:"images"`

	// Authorities the image must satisfy: at least one listed authority must verify
	// the image's signature. Omit to require attestations only (rare), or to gate on
	// nothing but presence — but a policy with neither authorities nor
	// requireAttestations verifies nothing and is rejected at admission.
	// +optional
	Authorities []VerificationAuthority `json:"authorities,omitempty"`

	// RequireAttestations lists predicate types the image must carry a verified
	// attestation for (SLSA provenance, an SBOM, a scan), each optionally fresh.
	// +optional
	RequireAttestations []AttestationRequirement `json:"requireAttestations,omitempty"`

	// Skip lists image globs this policy exempts — an audited escape valve for
	// images that cannot carry a new-format bundle (a third-party base image not yet
	// mirrored and re-signed). A skip is recorded, never silent.
	// +optional
	Skip []string `json:"skip,omitempty"`
}

// VerificationAuthority is one accepted signer: exactly one of keyless or key.
type VerificationAuthority struct {
	// Keyless matches a Fulcio certificate identity (issuer + subject), the Sigstore
	// keyless-signing model.
	// +optional
	Keyless *KeylessAuthority `json:"keyless,omitempty"`

	// Key verifies against a cosign public key held in a Secret.
	// +optional
	Key *KeyAuthority `json:"key,omitempty"`
}

// KeylessAuthority matches a Fulcio certificate's OIDC issuer and subject (SAN).
// Give issuer or issuerRegExp, and subject or subjectRegExp.
type KeylessAuthority struct {
	// Issuer is the exact OIDC issuer URL on the signing certificate.
	// +optional
	Issuer string `json:"issuer,omitempty"`
	// IssuerRegExp matches the issuer as a regular expression.
	// +optional
	IssuerRegExp string `json:"issuerRegExp,omitempty"`
	// Subject is the exact certificate SAN (e.g. the signing workflow identity).
	// +optional
	Subject string `json:"subject,omitempty"`
	// SubjectRegExp matches the SAN as a regular expression.
	// +optional
	SubjectRegExp string `json:"subjectRegExp,omitempty"`
}

// KeyAuthority verifies signatures with a cosign public key from a Secret.
type KeyAuthority struct {
	// SecretRef names the Secret holding the PEM public key (under key "cosign.pub").
	// +required
	SecretRef SecretReference `json:"secretRef"`
}

// SecretReference is a cluster-scoped Secret reference (namespace is required
// because the policy is cluster-scoped and cannot default it).
type SecretReference struct {
	// +required
	Namespace string `json:"namespace"`
	// +required
	Name string `json:"name"`
}

// AttestationRequirement requires a verified attestation of a predicate type,
// optionally within a freshness window.
type AttestationRequirement struct {
	// PredicateType is the in-toto predicate type that must be present and verified
	// (e.g. "https://slsa.dev/provenance/v1", "https://spdx.dev/Document").
	// +required
	PredicateType string `json:"predicateType"`

	// MaxAge, when set, requires the attestation to be no older than this — so a
	// stale vuln scan or provenance fails the gate.
	// +optional
	MaxAge *metav1.Duration `json:"maxAge,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=ivp
// +kubebuilder:printcolumn:name="Images",type=string,JSONPath=`.spec.images`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ImageVerificationPolicy is a cluster-scoped policy: images matching its globs must
// be signed by one of its authorities and carry its required attestations before a
// StageSet stage applies them. Verification is owned by the platform, so tenants
// deploy without declaring any signing configuration.
type ImageVerificationPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec ImageVerificationPolicySpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// ImageVerificationPolicyList is a list of ImageVerificationPolicy.
type ImageVerificationPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ImageVerificationPolicy `json:"items"`
}
