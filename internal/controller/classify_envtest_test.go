// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	fluxpatch "github.com/fluxcd/pkg/runtime/patch"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/artifact"
)

func readyMessageOf(ss *stagesv1.StageSet) string {
	c := apimeta.FindStatusCondition(ss.Status.Conditions, ConditionReady)
	if c == nil {
		return ""
	}
	return c.Message
}

// newClassifyReconciler creates a fresh envtest-backed reconciler + namespace +
// a getter for the live StageSet.
func newClassifyReconciler(t *testing.T) (*StageSetReconciler, func(ns, name string) *stagesv1.StageSet, string) {
	c := testClient(t)
	ns := newNamespace(t, c)
	r := &StageSetReconciler{Client: c, RESTMapper: c.RESTMapper()}
	get := func(ns, name string) *stagesv1.StageSet {
		var ss stagesv1.StageSet
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &ss); err != nil {
			t.Fatalf("get StageSet: %v", err)
		}
		return &ss
	}
	return r, get, ns
}

func createStageSetFor(t *testing.T, r *StageSetReconciler, ns, name string) *stagesv1.StageSet {
	t.Helper()
	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: stagesv1.StageSetSpec{
			Interval: metav1.Duration{Duration: time.Minute},
			Stages:   []stagesv1.Stage{{Name: "stage-a", SourceRef: stagesv1.SourceReference{Name: "ea"}}},
		},
	}
	if err := r.Client.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	return ss
}

func newHelperFor(t *testing.T, r *StageSetReconciler, ss *stagesv1.StageSet) *fluxpatch.Helper {
	t.Helper()
	h, err := fluxpatch.NewHelper(ss, r.Client)
	if err != nil {
		t.Fatalf("new helper: %v", err)
	}
	return h
}

func forbidden() error {
	return apierrors.NewForbidden(schema.GroupResource{Group: "source.toolkit.fluxcd.io", Resource: "externalartifacts"}, "ea",
		errors.New("serviceaccount cannot get resource"))
}

// A permanent API error (Forbidden) during resolution is terminal RBACDenied:
// no error is returned (so controller-runtime doesn't back off), but the
// reconcile requeues at the bounded permanentRetryInterval. Granting the tenant
// SA the missing RBAC fires no watch event the StageSet sees, so the bounded
// requeue is what lets it self-heal within ~1 minute instead of staying stuck
// until a manual reconcile.
func TestFailResolution_PermanentAPIError_TerminalRBACDenied(t *testing.T) {
	r, get, ns := newClassifyReconciler(t)
	ss := createStageSetFor(t, r, ns, "rbac")
	res, rerr := r.failResolution(context.Background(), newHelperFor(t, r, ss), ss, "stage-a",
		stagesv1.SourceReference{Name: "ea"}, ns, fmt.Errorf("get ExternalArtifact: %w", forbidden()))
	if rerr != nil {
		t.Fatalf("RBACDenied must not return an error (no backoff), got %v", rerr)
	}
	if res.RequeueAfter != permanentRetryInterval {
		t.Fatalf("RBACDenied must requeue at permanentRetryInterval for self-heal, got %+v", res)
	}
	if rr := readyReason(get(ns, "rbac")); rr != ReasonRBACDenied {
		t.Fatalf("Ready reason = %q, want %q", rr, ReasonRBACDenied)
	}
}

// A genuinely transient resolution error still requeues at the retry interval.
func TestFailResolution_Transient_Requeues(t *testing.T) {
	r, get, ns := newClassifyReconciler(t)
	ss := createStageSetFor(t, r, ns, "transient")
	res, rerr := r.failResolution(context.Background(), newHelperFor(t, r, ss), ss, "stage-a",
		stagesv1.SourceReference{Name: "ea"}, ns, fmt.Errorf("%w", artifact.ErrSourceNotReady))
	if rerr != nil {
		t.Fatalf("transient should not return an error here, got %v", rerr)
	}
	if res.RequeueAfter == 0 {
		t.Fatalf("transient SourceNotReady should requeue, got %+v", res)
	}
	if rr := readyReason(get(ns, "transient")); rr != ReasonSourceNotReady {
		t.Fatalf("Ready reason = %q, want %q", rr, ReasonSourceNotReady)
	}
}

// A cross-namespace resolution failure is scrubbed to the constant message: no
// NotFound/Forbidden/other-detail leak about the foreign namespace.
func TestFailResolution_CrossNamespace_Scrubbed(t *testing.T) {
	r, get, ns := newClassifyReconciler(t)
	ss := createStageSetFor(t, r, ns, "xns")
	ref := stagesv1.SourceReference{Kind: "GitRepository", Name: "app", Namespace: "other-team"}
	// Even a Forbidden (which would normally classify RBACDenied) must be scrubbed
	// when cross-namespace so it can't fingerprint the foreign namespace.
	_, _ = r.failResolution(context.Background(), newHelperFor(t, r, ss), ss, "stage-a", ref, ns,
		fmt.Errorf("get GitRepository: %w", forbidden()))
	msg := readyMessageOf(get(ns, "xns"))
	if !strings.Contains(msg, "is not reachable") || !strings.Contains(msg, "other-team") {
		t.Fatalf("cross-ns message should be scrubbed constant, got %q", msg)
	}
	for _, leak := range []string{"Forbidden", "forbidden", "cannot get", "NotFound"} {
		if strings.Contains(msg, leak) {
			t.Fatalf("cross-ns message leaks %q: %q", leak, msg)
		}
	}
}

// A terminal fetch error in failStage stops requeue; a transient one backs off.
func TestFailStage_TerminalFetch_NoRequeue(t *testing.T) {
	r, get, ns := newClassifyReconciler(t)
	ss := createStageSetFor(t, r, ns, "fetchterm")
	_, rerr := r.failStage(context.Background(), newHelperFor(t, r, ss), ss, "stage-a", "fetch artifact",
		fmt.Errorf("%w", artifact.ErrDigestMismatch), nil, "rev1", nil)
	// failStage marks terminal failures with the sentinel; the reconcile loop
	// unwraps it to a nil error (no requeue). Asserting the sentinel here pins
	// that contract.
	if !errors.Is(rerr, errTerminalStageFailure) {
		t.Fatalf("terminal fetch error must be marked terminal, got %v", rerr)
	}
	if rr := readyReason(get(ns, "fetchterm")); rr != ReasonStageFailed {
		t.Fatalf("Ready reason = %q, want %q", rr, ReasonStageFailed)
	}
}

// The reconcile loop unwraps a terminal stage failure to a nil error so
// controller-runtime does not requeue.
func TestReconcileLoop_TerminalUnwrapsToNoRequeue(t *testing.T) {
	if !errors.Is(fmt.Errorf("%w: %w", errTerminalStageFailure, artifact.ErrDigestMismatch), errTerminalStageFailure) {
		t.Fatal("the terminal sentinel must be unwrappable from the wrapped error")
	}
}

func TestFailStage_TransientFetch_BacksOff(t *testing.T) {
	r, _, ns := newClassifyReconciler(t)
	ss := createStageSetFor(t, r, ns, "fetchtrans")
	cause := errors.New("dial tcp: connection refused")
	_, rerr := r.failStage(context.Background(), newHelperFor(t, r, ss), ss, "stage-a", "fetch artifact", cause, nil, "rev1", nil)
	if !errors.Is(rerr, cause) {
		t.Fatalf("transient fetch error should return the cause for backoff, got %v", rerr)
	}
}

// A permanent API error in failStage (e.g. an impersonated apply Forbidden) is
// terminal RBACDenied — no requeue.
func TestFailStage_PermanentAPIError_TerminalRBACDenied(t *testing.T) {
	r, get, ns := newClassifyReconciler(t)
	ss := createStageSetFor(t, r, ns, "applyrbac")
	_, rerr := r.failStage(context.Background(), newHelperFor(t, r, ss), ss, "stage-a", "apply",
		fmt.Errorf("apply: %w", forbidden()), nil, "rev1", nil)
	if !errors.Is(rerr, errTerminalStageFailure) {
		t.Fatalf("RBACDenied apply must be marked terminal, got %v", rerr)
	}
	if rr := readyReason(get(ns, "applyrbac")); rr != ReasonRBACDenied {
		t.Fatalf("Ready reason = %q, want %q", rr, ReasonRBACDenied)
	}
}
