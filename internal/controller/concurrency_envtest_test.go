// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/artifact"
)

// The controller reconciles concurrentReconciles StageSets at once, through ONE
// reconciler value: every collaborator hanging off it — the token cache, the
// per-tenant target cache, the producer-watch maps, the metric querier's lazily
// built HTTP client — is shared across those goroutines. This drives that shape
// directly so `go test -race` has something to find if any of it stops being
// guarded.
//
// Distinct StageSets in distinct namespaces, because that is the case the
// concurrency exists for: one tenant's slow stage must not serialize behind
// another's, and neither may corrupt shared state on the way through.
func TestReconcile_ConcurrentStageSetsAreRaceFree(t *testing.T) {
	cfg := envtestConfig(t)
	scheme := testScheme(t)
	base, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("NewWithWatch: %v", err)
	}

	const tenants = 6
	reqs := make([]ctrl.Request, 0, tenants)
	for i := range tenants {
		ns := newNamespace(t, base)
		name := fmt.Sprintf("tenant-%d", i)
		servedArtifact(t, base, ns, "ea", "", map[string]string{
			"cm.yaml": configMapManifest(ns, name),
		})
		ss := &stagesv1.StageSet{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
			Spec: stagesv1.StageSetSpec{
				Interval: metav1.Duration{Duration: time.Minute},
				Stages: []stagesv1.Stage{{
					Name:      "only",
					SourceRef: stagesv1.SourceReference{Name: "ea"},
				}},
			},
		}
		if err := base.Create(context.Background(), ss); err != nil {
			t.Fatalf("create %s/%s: %v", ns, name, err)
		}
		reqs = append(reqs, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
	}

	// One reconciler for every goroutine, as the manager wires it.
	r := &StageSetReconciler{
		Client:     base,
		APIReader:  base,
		RESTMapper: base.RESTMapper(),
		Recorder:   &capturingRecorder{},
		Fetcher: &artifact.Fetcher{
			HTTPClient:   http.DefaultClient,
			URLValidator: artifact.PermissiveHTTPURL,
			IPValidator:  artifact.PermissiveIP,
		},
	}

	// Released together so the reconciles genuinely overlap rather than
	// queueing behind each other's setup.
	var start sync.WaitGroup
	start.Add(1)
	var done sync.WaitGroup
	errs := make([]error, len(reqs))
	for i, req := range reqs {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			// driveReconcile, because the first reconcile of a fresh StageSet
			// only adds the finalizer and requeues.
			_, errs[i] = driveReconcile(r, req)
		}()
	}
	start.Done()
	done.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("%s: %v", reqs[i].NamespacedName, err)
		}
	}
	// Every tenant converged, so the shared caches served all six correctly
	// rather than one goroutine's entry being handed to another.
	for _, req := range reqs {
		ss := getStageSet(t, base, req.Namespace, req.Name)
		if got := readyReason(ss); got != ReasonReady {
			t.Errorf("%s Ready reason = %q, want %q", req.NamespacedName, got, ReasonReady)
		}
	}
}

// concurrentReconciles is what the manager is wired with, and the reason it is
// above one is that a verify wait blocks its worker for the whole stage timeout.
// Pinned so a well-meant "make it configurable, default 1" has to argue with a
// test rather than slip through.
func TestConcurrentReconciles_IsAboveOne(t *testing.T) {
	t.Parallel()
	if concurrentReconciles <= 1 {
		t.Fatalf("concurrentReconciles = %d: a single worker lets one tenant's slow stage stall every other tenant for a full stage timeout (%s)",
			concurrentReconciles, defaultStageTimeout)
	}
}
