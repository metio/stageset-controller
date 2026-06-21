// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"errors"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/artifact"
)

// isPermanentAPIError reports whether err is an apiserver-side failure that
// retry cannot recover from:
//
//   - apierrors.IsForbidden — the SA making the call lacks the required verb.
//     The cluster operator (or whoever owns the tenant's RBAC) must grant it.
//   - apimeta.IsNoMatchError / runtime.IsNotRegisteredError — the resource kind
//     is not registered with the apiserver. The cluster operator must install
//     the CRD.
//
// Callers route these to terminal ReasonRBACDenied so the workqueue does not
// burn cycles on permanently-failing StageSets. Every other API error
// (NotFound, Conflict, transient transport failures) stays on the default
// "engage backoff" path.
//
// Schema-level rejections (IsInvalid / IsBadRequest) are deliberately NOT
// treated as permanent here: an immutable-field apply conflict surfaces as
// IsInvalid and is resolved by a per-stage conflictPolicy without a StageSet
// spec change, so it must stay on the retryable StageFailed path.
//
// Caveat: IsForbidden keys off HTTP 403, which in Kubernetes practice is always
// RBAC (quota → 429, admission rejection → 422). A degraded cluster could
// surface 403 for a non-RBAC reason; that misclassifies as permanent and stops
// retrying, but the status reflects the underlying error verbatim and the next
// genuine watch event re-triggers a reconcile.
func isPermanentAPIError(err error) bool {
	return apierrors.IsForbidden(err) ||
		apimeta.IsNoMatchError(err) ||
		runtime.IsNotRegisteredError(err)
}

// rbacDenialMessage builds the user-facing message for a permanent API error.
// The Forbidden path quotes the apiserver's verbatim error (which names the SA,
// verb, and resource), prefixed with a pointer to the tenant-RBAC docs. The
// NoMatch path names the missing kind so the operator knows which CRD to
// install. The `context` argument names the call that failed (e.g. "resolving
// the source CR").
func rbacDenialMessage(context string, err error) string {
	switch {
	case apierrors.IsForbidden(err):
		return "RBAC denied " + context + " — grant the missing verb to the tenant ServiceAccount (see the Tenancy and RBAC guide). " + err.Error()
	case apimeta.IsNoMatchError(err), runtime.IsNotRegisteredError(err):
		return context + " refers to a kind not registered with the apiserver — install the corresponding CRD. " + err.Error()
	default:
		return context + ": " + err.Error()
	}
}

// terminalFetchError reports whether err is a fetch failure that retry cannot
// recover from, so the caller stops engaging controller-runtime's backoff and
// waits for the next genuine watch event instead. Mirrors the resolver's
// transient/terminal split for the fetch phase:
//
//   - SSRF-defence rejections (urlguard sentinels) — the same URL is rejected
//     the same way next time.
//   - Integrity errors (digest mismatch / malformed digest) — a retry can't fix
//     corruption; the upstream must republish.
//   - Tarball-shape caps (oversized body / archive / entry / decompressed
//     stream) — the upstream must shrink or sanitize.
//   - Dial-time forbidden-address pinning — the host resolves to a denied
//     surface; retry resolves the same way.
//
// Network errors, 5xx, and source-not-ready are left out: those are genuinely
// transient and should requeue with backoff.
func terminalFetchError(err error) bool {
	return errors.Is(err, artifact.ErrInvalidScheme) ||
		errors.Is(err, artifact.ErrMissingHost) ||
		errors.Is(err, artifact.ErrForbiddenHost) ||
		errors.Is(err, artifact.ErrForbiddenAddress) ||
		errors.Is(err, artifact.ErrDigestInvalid) ||
		errors.Is(err, artifact.ErrDigestMismatch) ||
		errors.Is(err, artifact.ErrArtifactBodyTooLarge) ||
		errors.Is(err, artifact.ErrTarballTooLarge) ||
		errors.Is(err, artifact.ErrDecompressedTooLarge) ||
		errors.Is(err, artifact.ErrDuplicateEntry)
}

// transientDecryptSignatures are the substrings cloud-KMS SDKs surface for a
// rate-limit / throttle / temporary-service condition. A decrypt failure that
// matches one is treated as transient (back off and retry) rather than terminal,
// so a cloud-KMS throttle during a rollback does not get reported as a permanent
// PreviousRevisionUnavailable.
//
// The SOPS decryptor surfaces the underlying cloud error verbatim without a typed
// wrapper, so substring matching against each provider's documented throttle
// strings is the cleanest available split. The match is case-insensitive and
// errs toward terminal: an auth/key-policy failure (the common case) does not
// match any of these and stays terminal. The trade-off is a throttle string a
// provider renames in future falls back to terminal — safe, since the next
// genuine reconcile re-attempts the rollback.
var transientDecryptSignatures = []string{
	"throttl",             // AWS ThrottlingException, generic "throttled"
	"rate exceeded",       // AWS RequestLimitExceeded / "Rate exceeded"
	"too many requests",   // HTTP 429 phrasing (Azure / GCP)
	"resourceexhausted",   // gRPC ResourceExhausted (GCP KMS)
	"resource_exhausted",  //   alternate gRPC rendering
	"quota",               // GCP "Quota exceeded"
	"serviceunavailable",  // transient service outage
	"service unavailable", //   spaced rendering
	"requesttimeout",      // transient timeout
	"request timeout",     //   spaced rendering
	"slowdown",            // S3-style backpressure
	"temporarily",         // "temporarily unavailable"
	"try again",           // generic retryable hint
}

// isTransientDecryptError reports whether a decrypt failure looks like a
// transient cloud-KMS condition (throttle / rate limit / temporary outage)
// rather than a terminal auth or key-policy denial. See transientDecryptSignatures
// for the classification basis and its limitation.
func isTransientDecryptError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, sig := range transientDecryptSignatures {
		if strings.Contains(msg, sig) {
			return true
		}
	}
	return false
}

// isCrossNamespaceRef reports whether ref names a namespace other than ownerNS.
// An empty ref.Namespace defaults to ownerNS (the StageSet's own namespace), so
// this only fires on an explicit out-of-namespace reference.
func isCrossNamespaceRef(ref stagesv1.SourceReference, ownerNS string) bool {
	return ref.Namespace != "" && ref.Namespace != ownerNS
}

// scrubbedCrossNamespaceMessage replaces a cross-namespace resolution / fetch
// failure's raw message with a constant string that names only what the tenant
// already knows (the kind + name they wrote on their own CR's spec, plus the
// target namespace they themselves specified). It DOES NOT include the
// underlying error's text — which would distinguish NotFound / Forbidden /
// digest-mismatch / 5xx — so a tenant cannot fingerprint another namespace's
// state. Operators investigating check the source CR's status in the target
// namespace, not the StageSet's status.
func scrubbedCrossNamespaceMessage(ref stagesv1.SourceReference, ns string) string {
	kind := ref.Kind
	if kind == "" {
		kind = "ExternalArtifact"
	}
	return fmt.Sprintf("cross-namespace %s %q is not reachable; check the source CR's status in %q",
		kind, ref.Name, ns)
}
