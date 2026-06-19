// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/artifact"
)

func forbiddenErr() error {
	return apierrors.NewForbidden(schema.GroupResource{Group: "source.toolkit.fluxcd.io", Resource: "externalartifacts"}, "ea",
		errors.New("serviceaccount cannot get resource"))
}

func noMatchErr() error {
	return &apimeta.NoResourceMatchError{PartialResource: schema.GroupVersionResource{Group: "g", Version: "v", Resource: "things"}}
}

func TestIsPermanentAPIError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"forbidden", forbiddenErr(), true},
		{"nomatch", noMatchErr(), true},
		// IsInvalid (e.g. an immutable-field apply conflict) is intentionally
		// retryable here, resolved by a conflictPolicy — not permanent.
		{"invalid is retryable", apierrors.NewInvalid(schema.GroupKind{Kind: "X"}, "x", nil), false},
		{"badrequest is retryable", apierrors.NewBadRequest("nope"), false},
		{"notfound is transient", apierrors.NewNotFound(schema.GroupResource{Resource: "x"}, "x"), false},
		{"conflict is transient", apierrors.NewConflict(schema.GroupResource{Resource: "x"}, "x", errors.New("c")), false},
		{"plain error is transient", errors.New("dial tcp: timeout"), false},
		{"wrapped forbidden", fmt.Errorf("get ea: %w", forbiddenErr()), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isPermanentAPIError(tc.err); got != tc.want {
				t.Fatalf("isPermanentAPIError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestRBACDenialMessage(t *testing.T) {
	t.Parallel()
	if msg := rbacDenialMessage("resolving the source CR", forbiddenErr()); !strings.Contains(msg, "RBAC denied resolving the source CR") {
		t.Fatalf("forbidden message missing context: %q", msg)
	}
	if msg := rbacDenialMessage("apply", noMatchErr()); !strings.Contains(msg, "not registered with the apiserver") {
		t.Fatalf("nomatch message missing CRD hint: %q", msg)
	}
}

func TestTerminalFetchError(t *testing.T) {
	t.Parallel()
	terminal := []error{
		artifact.ErrInvalidScheme,
		artifact.ErrMissingHost,
		artifact.ErrForbiddenHost,
		artifact.ErrForbiddenAddress,
		artifact.ErrDigestInvalid,
		artifact.ErrDigestMismatch,
		artifact.ErrArtifactBodyTooLarge,
		artifact.ErrTarballTooLarge,
		artifact.ErrDecompressedTooLarge,
	}
	for _, e := range terminal {
		if !terminalFetchError(fmt.Errorf("fetch %s: %w", "x", e)) {
			t.Errorf("expected %v to be terminal", e)
		}
	}
	transient := []error{
		errors.New("dial tcp: connection refused"),
		artifact.ErrSourceNotReady,
		artifact.ErrArtifactMissing,
	}
	for _, e := range transient {
		if terminalFetchError(e) {
			t.Errorf("expected %v to be transient", e)
		}
	}
}

func TestIsCrossNamespaceRef(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		ref   stagesv1.SourceReference
		owner string
		want  bool
	}{
		{"empty ns defaults to owner", stagesv1.SourceReference{Name: "ea"}, "team-a", false},
		{"same ns explicit", stagesv1.SourceReference{Name: "ea", Namespace: "team-a"}, "team-a", false},
		{"different ns", stagesv1.SourceReference{Name: "ea", Namespace: "team-b"}, "team-a", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isCrossNamespaceRef(tc.ref, tc.owner); got != tc.want {
				t.Fatalf("isCrossNamespaceRef = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestScrubbedCrossNamespaceMessage(t *testing.T) {
	t.Parallel()
	ref := stagesv1.SourceReference{Kind: "GitRepository", Name: "app", Namespace: "team-b"}
	msg := scrubbedCrossNamespaceMessage(ref, ref.Namespace)
	// The message names only what the tenant already wrote: kind, name, target ns.
	for _, want := range []string{"GitRepository", `"app"`, `"team-b"`, "is not reachable"} {
		if !strings.Contains(msg, want) {
			t.Errorf("scrubbed message %q missing %q", msg, want)
		}
	}
	// It must NOT distinguish failure modes — no NotFound/Forbidden/digest text.
	for _, leak := range []string{"NotFound", "Forbidden", "forbidden", "not found", "digest", "403", "404"} {
		if strings.Contains(msg, leak) {
			t.Errorf("scrubbed message %q leaks %q", msg, leak)
		}
	}
	// Empty kind defaults to ExternalArtifact.
	if m := scrubbedCrossNamespaceMessage(stagesv1.SourceReference{Name: "x", Namespace: "n"}, "n"); !strings.Contains(m, "ExternalArtifact") {
		t.Errorf("empty kind should default to ExternalArtifact, got %q", m)
	}
}

func TestIsTransientDecryptError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"aws throttling", errors.New("operation error KMS: Decrypt, ThrottlingException: Rate exceeded"), true},
		{"gcp resource exhausted", errors.New("rpc error: code = ResourceExhausted desc = Quota exceeded"), true},
		{"azure 429", fmt.Errorf("decrypt %q: %w", "secret.yaml", errors.New("StatusCode=429 too many requests")), true},
		{"service unavailable", errors.New("KMS service unavailable, try again later"), true},
		{"s3 slowdown", errors.New("SlowDown: please reduce your request rate"), true},
		// Auth / key-policy denials are terminal: they do not match any throttle
		// signature.
		{"access denied terminal", errors.New("AccessDeniedException: not authorized to perform kms:Decrypt"), false},
		{"key disabled terminal", errors.New("DisabledException: the key is disabled"), false},
		{"no key material terminal", errors.New("no matching age identity for recipient"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isTransientDecryptError(tc.err); got != tc.want {
				t.Fatalf("isTransientDecryptError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
