// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/artifact"
)

// errStore is a RollbackStore whose Get returns a configurable (data, found, err).
type errStore struct {
	data  []byte
	found bool
	err   error
}

func (e *errStore) Put(context.Context, string, []byte) error { return nil }
func (e *errStore) Get(context.Context, string) ([]byte, bool, error) {
	return e.data, e.found, e.err
}

// capturingRecorder records the (type, reason, note) of every event. The note
// is fully formatted — the controller's event helper passes a "%s" format with
// the message in args, so storing the raw format would record a literal "%s".
type capturingRecorder struct {
	mu     sync.Mutex
	events []recordedEvent
}

type recordedEvent struct{ etype, reason, note string }

func (c *capturingRecorder) Eventf(_ runtime.Object, _ runtime.Object, etype, reason, _ string, note string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, recordedEvent{etype, reason, fmt.Sprintf(note, args...)})
}

func (c *capturingRecorder) has(reason string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.events {
		if e.reason == reason {
			return true
		}
	}
	return false
}

func rollbackTestStageSet() *stagesv1.StageSet {
	return &stagesv1.StageSet{
		Spec: stagesv1.StageSetSpec{
			Stages: []stagesv1.Stage{{Name: "stage-a"}},
		},
	}
}

// A transient rollback-store outage on Get must surface as an error (so the
// reconcile backs off) and must NOT fall through to a producer re-fetch that
// could mislabel the failure as terminal PreviousRevisionUnavailable.
func TestRollbackStageObjects_TransientStoreError_BacksOff(t *testing.T) {
	rec := &capturingRecorder{}
	r := &StageSetReconciler{
		Recorder:      rec,
		RollbackStore: &errStore{err: errors.New("s3: connection reset")},
	}
	ss := rollbackTestStageSet()
	ref := stagesv1.StageArtifactRef{Stage: "stage-a", URL: "http://example.invalid/x.tar.gz", Digest: "sha256:abc", Revision: "r1"}
	// A fetcher that would be reached only if the store path wrongly fell through.
	fetcher := &artifact.Fetcher{HTTPClient: http.DefaultClient, URLValidator: artifact.PermissiveHTTPURL, IPValidator: artifact.PermissiveIP}

	objs, reason, msg, err := r.rollbackStageObjects(context.Background(), ss, &ss.Spec.Stages[0], ref, fetcher, nil)
	if err == nil {
		t.Fatalf("transient store error must surface as an error to back off; got objs=%v reason=%q msg=%q", objs, reason, msg)
	}
	if reason == ReasonPreviousRevisionUnavailable {
		t.Fatalf("transient store error must not be reported as terminal PreviousRevisionUnavailable")
	}
	if !strings.Contains(err.Error(), "rollback store get") {
		t.Fatalf("error should name the rollback-store get: %v", err)
	}
	if !rec.has("RollbackStoreFailed") {
		t.Fatalf("transient store error should emit a RollbackStoreFailed event")
	}
}

// A corrupt snapshot (Get succeeds, decode fails) must be surfaced via an event
// and fall through to the producer re-fetch rather than silently succeeding.
func TestRollbackStageObjects_CorruptSnapshot_EventsAndFallsThrough(t *testing.T) {
	rec := &capturingRecorder{}
	r := &StageSetReconciler{
		Recorder:      rec,
		RollbackStore: &errStore{data: []byte("{ this is not valid json"), found: true},
	}
	ss := rollbackTestStageSet()
	ref := stagesv1.StageArtifactRef{Stage: "stage-a", URL: "http://gone.invalid/x.tar.gz", Digest: "sha256:abc", Revision: "r1"}
	// The producer is unreachable, so the fall-through fetch fails — proving the
	// corrupt snapshot did NOT short-circuit to success.
	fetcher := &artifact.Fetcher{HTTPClient: http.DefaultClient, URLValidator: artifact.PermissiveHTTPURL, IPValidator: artifact.PermissiveIP}

	_, reason, msg, err := r.rollbackStageObjects(context.Background(), ss, &ss.Spec.Stages[0], ref, fetcher, nil)
	if err != nil {
		t.Fatalf("corrupt snapshot is non-transient; it must not surface as a backoff error: %v", err)
	}
	if reason != ReasonPreviousRevisionUnavailable {
		t.Fatalf("corrupt snapshot should fall through to the producer re-fetch which fails terminally; got reason=%q msg=%q", reason, msg)
	}
	if !rec.has("RollbackStoreFailed") {
		t.Fatalf("corrupt snapshot should emit a RollbackStoreFailed event")
	}
}
