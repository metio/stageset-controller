// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/fluxcd/cli-utils/pkg/object"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

// verifyHeartbeatInterval is how often a verify wait that is still blocking
// reports the time it has spent. A stage that becomes ready within the interval
// logs nothing extra, so the line costs nothing on a healthy fleet and appears
// exactly when a wait is long enough to need explaining: the wait holds its
// worker for the whole stage timeout — fifteen minutes by default — and silence
// across that span is indistinguishable from a controller that has stopped.
const verifyHeartbeatInterval = time.Minute

// deletionPollInterval is how often a blocking verify wait re-reads its StageSet
// to notice a deletion. The read is served from the informer cache, so a stage's
// whole verify wait costs one cache lookup per interval.
const deletionPollInterval = 5 * time.Second

// stageWaiter is the readiness half of the applier: the blocking kstatus wait,
// and the terminal-failure question a timed-out wait leaves open. *apply.Applier
// implements it; tests substitute a fake so the hold-vs-fail-vs-abandon decision
// is exercised without a cluster.
type stageWaiter interface {
	Wait(ctx context.Context, set object.ObjMetadataSet, timeout time.Duration) error
	Stalled(ctx context.Context, set object.ObjMetadataSet) bool
}

// verifyOutcome is the result of verifying one stage's readiness.
type verifyOutcome struct {
	// cause is nil once the stage is ready; otherwise it names what the stage is
	// still waiting on, decorated with the bound that was hit.
	cause error
	// progressing separates a stage that ran out of clock while its objects were
	// still converging from one that reached a terminal failure. The former holds,
	// the latter fails.
	progressing bool
	// deleted reports that the StageSet acquired a deletionTimestamp mid-wait, so
	// the gates were abandoned rather than answered.
	deleted bool
}

// verifyStage runs a stage's readiness gates: the kstatus wait over the applied
// set (unless DisableWait) plus any explicit ReadyChecks.Checks, then the CEL
// ReadyChecks.Exprs over the live state of the applied objects. All gates share
// the stage's verify timeout, and Stalled is asked on the parent context so the
// hold-vs-fail question is still answerable when the gates' own context is spent.
func (r *StageSetReconciler) verifyStage(
	ctx context.Context,
	ss *stagesv1.StageSet,
	stage *stagesv1.Stage,
	waiter stageWaiter,
	target client.Client,
	waitSet object.ObjMetadataSet,
	objects []*unstructured.Unstructured,
	timeout time.Duration,
) verifyOutcome {
	logger := log.FromContext(ctx)
	deadline := r.now().Add(timeout)
	logger.V(1).Info("verifying stage readiness",
		"stage", stage.Name, "objects", len(waitSet),
		"timeout", timeout.String(), "deadline", deadline.UTC().Format(time.RFC3339))

	vctx, watch := r.watchForDeletion(ctx, client.ObjectKeyFromObject(ss))
	defer watch.Stop()
	stopReporting := heartbeat(vctx, verifyHeartbeatInterval, func(elapsed time.Duration) {
		logger.Info("stage is still becoming ready; verify wait continues",
			"stage", stage.Name, "elapsed", elapsed.Round(time.Second).String(),
			"timeout", timeout.String(), "deadline", deadline.UTC().Format(time.RFC3339))
	})
	defer stopReporting()

	if len(waitSet) > 0 {
		if err := waiter.Wait(vctx, waitSet, timeout); err != nil {
			if watch.Fired() {
				return verifyOutcome{deleted: true}
			}
			// The wait fails fast on a terminal kstatus failure, so if nothing is
			// Failed the clock simply ran out on objects that were still
			// progressing. Name the bound that was hit — the upstream message says
			// only what it was waiting for, which sends operators to raise a
			// client-side wait that can never help.
			cause := err
			progressing := !waiter.Stalled(ctx, waitSet)
			if progressing {
				cause = fmt.Errorf("%w (waited %s: stage readyChecks.timeout, else stages[].timeout, else spec.timeout)", err, timeout)
			}
			return verifyOutcome{cause: cause, progressing: progressing}
		}
	}
	if err := evalReadyExprs(vctx, target, ss, stage, objects, timeout); err != nil {
		if watch.Fired() {
			return verifyOutcome{deleted: true}
		}
		// A CEL-gated object that says it is still in progress gets the same
		// treatment as a kstatus object that is not Failed.
		return verifyOutcome{cause: err, progressing: errors.Is(err, errReadyExprsProgressing)}
	}
	return verifyOutcome{}
}

// deletionWatch cancels a derived context once the StageSet it guards carries a
// deletionTimestamp.
//
// controller-runtime dispatches at most one reconcile per object key, so a
// worker parked in a stage's verify wait also parks every event for that
// StageSet — including its own deletion. Without this, deleting a StageSet whose
// stage is mid-wait costs the rest of the stage timeout before teardown can even
// begin, and the StageSet holds its finalizer (and its namespace in Terminating)
// for that whole span. Watching for the timestamp bounds it to one poll.
type deletionWatch struct {
	cancel context.CancelFunc
	done   chan struct{}
	fired  atomic.Bool
}

// watchForDeletion derives a context that is cancelled once key carries a
// deletionTimestamp. Read errors are ignored: a transient failure to read the
// StageSet is not evidence of a deletion, and must not abandon a healthy wait.
func (r *StageSetReconciler) watchForDeletion(ctx context.Context, key client.ObjectKey) (context.Context, *deletionWatch) {
	interval := r.deletionPoll
	if interval <= 0 {
		interval = deletionPollInterval
	}
	ctx, cancel := context.WithCancel(ctx)
	w := &deletionWatch{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(w.done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				var cur stagesv1.StageSet
				if err := r.Get(ctx, key, &cur); err != nil {
					continue
				}
				if !cur.DeletionTimestamp.IsZero() {
					w.fired.Store(true)
					cancel()
					return
				}
			}
		}
	}()
	return ctx, w
}

// Fired reports whether the watch cancelled the context because the StageSet is
// being deleted, as opposed to the context ending for any other reason.
func (w *deletionWatch) Fired() bool { return w.fired.Load() }

// Stop releases the watch and blocks until its goroutine has exited.
func (w *deletionWatch) Stop() {
	w.cancel()
	<-w.done
}

// heartbeat calls report every interval, with the time elapsed since this call,
// until the returned stop is called or ctx ends. Work that finishes inside the
// first interval reports nothing. stop blocks until the goroutine has exited, so
// report never runs after stop returns.
func heartbeat(ctx context.Context, interval time.Duration, report func(elapsed time.Duration)) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	started := time.Now()
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				report(now.Sub(started))
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}
