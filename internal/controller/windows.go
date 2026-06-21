// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"fmt"
	"time"

	fluxpatch "github.com/fluxcd/pkg/runtime/patch"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/artifact"
	"github.com/metio/stageset-controller/internal/metrics"
	"github.com/metio/stageset-controller/internal/window"
)

// updateNowAnnotation forces a held rollout through regardless of update
// windows, one-shot per value (tracked in status.lastHandledUpdateOverride).
const updateNowAnnotation = "stages.metio.wtf/update-now"

func (r *StageSetReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// gateUpdateWindows decides whether this run may proceed. It returns
// deferred=true (with a requeue result) when an update window holds the
// rollout; the caller then returns without applying. It maintains
// status.pendingUpdate and honors the one-shot update-now override.
func (r *StageSetReconciler) gateUpdateWindows(ctx context.Context, helper *fluxpatch.Helper, ss *stagesv1.StageSet, resolved []artifact.ResolvedArtifact) (ctrl.Result, bool, error) {
	// Emergency override: apply the held rollout once, regardless of windows.
	if token := ss.Annotations[updateNowAnnotation]; token != "" && token != ss.Status.LastHandledUpdateOverride {
		ss.Status.LastHandledUpdateOverride = token
		ss.Status.PendingUpdate = nil
		return ctrl.Result{}, false, nil
	}
	if len(ss.Spec.UpdateWindows) == 0 {
		ss.Status.PendingUpdate = nil
		return ctrl.Result{}, false, nil
	}

	allowed, nextChange, err := window.Decision(ss.Spec.UpdateWindows, r.now())
	if err != nil {
		// Malformed windows normally fail admission; this is the fallback.
		r.setReady(ss, metav1.ConditionFalse, ReasonInvalidSpec, fmt.Sprintf("update window: %v", err))
		ss.Status.ObservedGeneration = ss.Generation
		return ctrl.Result{}, true, r.patchStatus(ctx, helper, ss)
	}

	newRevision := false
	revisions := make(map[string]string, len(resolved))
	for _, ra := range resolved {
		revisions[ra.Key()] = ra.Revision
		if ss.Status.LastAppliedRevisions[ra.Key()] != ra.Revision {
			newRevision = true
		}
	}

	// windowScope=All freezes everything; the default holds only new rollouts.
	if allowed || (!newRevision && ss.Spec.WindowScope != "All") {
		ss.Status.PendingUpdate = nil
		return ctrl.Result{}, false, nil
	}

	// Whether we were already deferring before this reconcile. The event +
	// counter fire only on the transition into the deferred state, not on every
	// requeue while the window stays closed — a multi-day window requeues hourly,
	// which would otherwise spam identical events and turn the deferral counter
	// into a re-check counter.
	wasDeferring := ss.Status.PendingUpdate != nil

	pu := &stagesv1.PendingUpdate{}
	if newRevision {
		pu.Revisions = revisions
	}
	if !nextChange.IsZero() {
		pu.NextWindowOpens = &metav1.Time{Time: nextChange}
	}
	ss.Status.PendingUpdate = pu
	ss.Status.ObservedGeneration = ss.Generation

	msg := "delivery held by update window" + nextWindowSuffix(nextChange)
	if len(ss.Status.LastAppliedRevisions) > 0 {
		// Already deployed: the current state is healthy; this is a deliberate
		// wait, not a failure.
		r.setReady(ss, metav1.ConditionTrue, ReasonReady, "Deployed; "+msg)
	} else {
		r.setReady(ss, metav1.ConditionFalse, ReasonUpdateDeferred, msg)
	}
	if !wasDeferring {
		r.event(ss, corev1.EventTypeNormal, ReasonUpdateDeferred, msg)
		metrics.UpdateDeferredTotal.WithLabelValues(ss.Namespace, ss.Name).Inc()
	}
	if uerr := r.patchStatus(ctx, helper, ss); uerr != nil {
		return ctrl.Result{}, true, uerr
	}
	return ctrl.Result{RequeueAfter: requeueForWindow(nextChange, r.now(), r.retryInterval(ss))}, true, nil
}

func nextWindowSuffix(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return fmt.Sprintf(" (next window opens %s)", t.UTC().Format(time.RFC3339))
}

// requeueForWindow requeues at the next window boundary, clamped so a far-off
// absolute freeze re-checks periodically rather than sleeping across clock
// changes, and a near boundary does not hot-loop.
func requeueForWindow(nextChange, now time.Time, fallback time.Duration) time.Duration {
	if nextChange.IsZero() {
		return fallback
	}
	d := nextChange.Sub(now)
	switch {
	case d < 5*time.Second:
		return 5 * time.Second
	case d > time.Hour:
		return time.Hour
	default:
		return d
	}
}
