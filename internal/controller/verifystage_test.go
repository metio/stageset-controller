// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fluxcd/cli-utils/pkg/object"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

// fakeWaiter stands in for *apply.Applier's readiness half. block makes Wait
// park until its context ends, which is how a real kstatus wait behaves for a
// workload that never becomes ready.
type fakeWaiter struct {
	err     error
	stalled bool
	block   bool
	waited  atomic.Int64
}

func (f *fakeWaiter) Wait(ctx context.Context, _ object.ObjMetadataSet, _ time.Duration) error {
	f.waited.Add(1)
	if f.block {
		<-ctx.Done()
		return ctx.Err()
	}
	return f.err
}

func (f *fakeWaiter) Stalled(context.Context, object.ObjMetadataSet) bool { return f.stalled }

func verifyFixture(t *testing.T, deleting bool, poll time.Duration) (*StageSetReconciler, *stagesv1.StageSet) {
	t.Helper()
	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "ss"},
		Spec: stagesv1.StageSetSpec{
			Stages: []stagesv1.Stage{{Name: "one", SourceRef: stagesv1.SourceReference{Name: "src"}}},
		},
	}
	live := ss.DeepCopy()
	if deleting {
		live.Finalizers = []string{FinalizerName}
		live.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	}
	r := &StageSetReconciler{
		Client:       fake.NewClientBuilder().WithScheme(builderScheme(t)).WithObjects(live).Build(),
		deletionPoll: poll,
	}
	return r, ss
}

// A stage whose objects are still converging when the clock runs out holds; the
// cause names the bound that was hit, so an operator is not sent to raise a
// client-side wait that can never help.
func TestVerifyStage_TimedOutWhileProgressingHolds(t *testing.T) {
	t.Parallel()
	r, ss := verifyFixture(t, false, time.Hour)
	w := &fakeWaiter{err: errors.New("timeout waiting for: DaemonSet/ns/mirror"), stalled: false}

	got := r.verifyStage(context.Background(), ss, &ss.Spec.Stages[0], w, r.Client,
		object.ObjMetadataSet{{Name: "mirror", Namespace: "ns"}}, nil, 15*time.Minute)

	if got.deleted {
		t.Fatal("outcome reports a deletion that did not happen")
	}
	if !got.progressing {
		t.Error("a wait that timed out with nothing stalled must report progressing")
	}
	if !strings.Contains(got.cause.Error(), "waited 15m0s") {
		t.Errorf("cause does not name the bound that was hit: %v", got.cause)
	}
}

// A terminal kstatus failure is not a hold: the outcome carries the raw cause so
// the caller fails the stage (and may roll it back).
func TestVerifyStage_StalledObjectFails(t *testing.T) {
	t.Parallel()
	r, ss := verifyFixture(t, false, time.Hour)
	w := &fakeWaiter{err: errors.New("failed early: Job/ns/migrate"), stalled: true}

	got := r.verifyStage(context.Background(), ss, &ss.Spec.Stages[0], w, r.Client,
		object.ObjMetadataSet{{Name: "migrate", Namespace: "ns"}}, nil, time.Minute)

	if got.progressing || got.deleted {
		t.Fatalf("stalled object must fail outright, got %+v", got)
	}
	if strings.Contains(got.cause.Error(), "waited") {
		t.Errorf("a stalled object's cause must not blame the clock: %v", got.cause)
	}
}

// The point of the deletion watch: a StageSet deleted while a stage is mid-wait
// abandons the wait instead of holding the workqueue key for the rest of the
// stage timeout, which is what keeps its namespace in Terminating.
func TestVerifyStage_DeletionMidWaitAbandons(t *testing.T) {
	t.Parallel()
	r, ss := verifyFixture(t, true, time.Millisecond)
	w := &fakeWaiter{block: true}

	done := make(chan verifyOutcome, 1)
	go func() {
		done <- r.verifyStage(context.Background(), ss, &ss.Spec.Stages[0], w, r.Client,
			object.ObjMetadataSet{{Name: "mirror", Namespace: "ns"}}, nil, time.Hour)
	}()

	select {
	case got := <-done:
		if !got.deleted {
			t.Fatalf("wait returned without reporting the deletion: %+v", got)
		}
		if got.cause != nil {
			t.Errorf("an abandoned wait must not report a stage cause: %v", got.cause)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("verify wait did not return after the StageSet was deleted")
	}
}

// A wait with no objects to watch and no CEL exprs is ready immediately — the
// waiter is never called.
func TestVerifyStage_EmptyWaitSetIsReady(t *testing.T) {
	t.Parallel()
	r, ss := verifyFixture(t, false, time.Hour)
	w := &fakeWaiter{err: errors.New("must not be called")}

	got := r.verifyStage(context.Background(), ss, &ss.Spec.Stages[0], w, r.Client, nil, nil, time.Minute)

	if got.cause != nil || got.deleted || got.progressing {
		t.Fatalf("empty wait set must verify clean, got %+v", got)
	}
	if n := w.waited.Load(); n != 0 {
		t.Errorf("waiter called %d times for an empty wait set", n)
	}
}

func TestWatchForDeletion_LiveStageSetKeepsWaiting(t *testing.T) {
	t.Parallel()
	r, ss := verifyFixture(t, false, time.Millisecond)

	ctx, w := r.watchForDeletion(context.Background(), client.ObjectKeyFromObject(ss))
	defer w.Stop()

	select {
	case <-ctx.Done():
		t.Fatal("context cancelled for a StageSet that is not being deleted")
	case <-time.After(50 * time.Millisecond):
	}
	if w.Fired() {
		t.Error("watch fired without a deletionTimestamp")
	}
}

// A read failure is not evidence of a deletion: the watch keeps quiet rather
// than abandoning a healthy wait.
func TestWatchForDeletion_UnreadableStageSetKeepsWaiting(t *testing.T) {
	t.Parallel()
	r := &StageSetReconciler{
		Client:       fake.NewClientBuilder().WithScheme(builderScheme(t)).Build(),
		deletionPoll: time.Millisecond,
	}

	ctx, w := r.watchForDeletion(context.Background(), client.ObjectKeyFromObject(&stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "gone"},
	}))
	defer w.Stop()

	select {
	case <-ctx.Done():
		t.Fatal("context cancelled on a read error")
	case <-time.After(50 * time.Millisecond):
	}
	if w.Fired() {
		t.Error("watch fired on a read error")
	}
}

func TestHeartbeat(t *testing.T) {
	t.Parallel()

	t.Run("reports while the work runs", func(t *testing.T) {
		t.Parallel()
		beats := make(chan time.Duration, 8)
		stop := heartbeat(context.Background(), time.Millisecond, func(elapsed time.Duration) {
			select {
			case beats <- elapsed:
			default:
			}
		})
		defer stop()

		for range 3 {
			select {
			case elapsed := <-beats:
				if elapsed <= 0 {
					t.Errorf("elapsed = %v, want a positive duration", elapsed)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("heartbeat stopped reporting")
			}
		}
	})

	t.Run("silent for work that finishes inside the interval", func(t *testing.T) {
		t.Parallel()
		var beats atomic.Int64
		stop := heartbeat(context.Background(), time.Hour, func(time.Duration) { beats.Add(1) })
		stop()
		if got := beats.Load(); got != 0 {
			t.Errorf("reported %d times before the first interval elapsed", got)
		}
	})

	t.Run("stop blocks until reporting has ended", func(t *testing.T) {
		t.Parallel()
		var beats atomic.Int64
		stop := heartbeat(context.Background(), time.Millisecond, func(time.Duration) { beats.Add(1) })
		time.Sleep(20 * time.Millisecond)
		stop()
		settled := beats.Load()
		time.Sleep(20 * time.Millisecond)
		if got := beats.Load(); got != settled {
			t.Errorf("report ran after stop returned (%d -> %d)", settled, got)
		}
	})
}
