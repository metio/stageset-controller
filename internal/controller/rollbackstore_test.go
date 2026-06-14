// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"net/http"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/apply"
	"github.com/metio/stageset-controller/internal/artifact"
)

type fakeRollbackStore struct{ data map[string][]byte }

func newFakeStore() *fakeRollbackStore { return &fakeRollbackStore{data: map[string][]byte{}} }

func (f *fakeRollbackStore) Put(_ context.Context, key string, data []byte) error {
	f.data[key] = data
	return nil
}

func (f *fakeRollbackStore) Get(_ context.Context, key string) ([]byte, bool, error) {
	d, ok := f.data[key]
	return d, ok, nil
}

func uConfigMap(ns, name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("v1")
	u.SetKind("ConfigMap")
	u.SetNamespace(ns)
	u.SetName(name)
	_ = unstructured.SetNestedField(u.Object, "stored", "data", "key")
	return u
}

// A successful run pushes its rendered output to the external store.
func TestReconcile_RollbackStore_StoresOnSuccess(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	servedArtifact(t, c, ns, "ea", "", map[string]string{"cm.yaml": configMapManifest(ns, "obj")})
	store := newFakeStore()

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "store-ok"},
		Spec: stagesv1.StageSetSpec{
			Interval:          metav1.Duration{Duration: time.Minute},
			RollbackOnFailure: true,
			Stages:            []stagesv1.Stage{{Name: "stage-a", SourceRef: stagesv1.SourceReference{Name: "ea"}}},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	r := &StageSetReconciler{
		Client:        c,
		RESTMapper:    c.RESTMapper(),
		Fetcher:       &artifact.Fetcher{HTTPClient: http.DefaultClient, URLValidator: artifact.PermissiveHTTPURL, IPValidator: artifact.PermissiveIP},
		RollbackStore: store,
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ss)}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(store.data) == 0 {
		t.Fatal("a successful run with rollbackOnFailure + a store should have pushed rendered output")
	}
}

// With the store holding the rendered output, rollback succeeds even when the
// producer artifact is gone (the fetcher fails) — bit-exact, GC-independent.
func TestAttemptRollback_StoreSurvivesProducerGC(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	store := newFakeStore()

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "store-gc"},
		Spec: stagesv1.StageSetSpec{
			Interval:          metav1.Duration{Duration: time.Minute},
			RollbackOnFailure: true,
			Stages:            []stagesv1.Stage{{Name: "stage-a", SourceRef: stagesv1.SourceReference{Name: "ea"}}},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	// Record a snapshot whose producer URL is unreachable, but stage the
	// rendered output in the store keyed by its digest.
	ref := stagesv1.StageArtifactRef{Stage: "stage-a", URL: "http://gone.invalid/x.tar.gz", Digest: "sha256:deadbeef", Revision: "r1"}
	ss.Status.LastAppliedSnapshot = []stagesv1.StageArtifactRef{ref}
	data, err := encodeObjects([]*unstructured.Unstructured{uConfigMap(ns, "restored")})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	store.data[rollbackKey(ss, ref.Stage, ref.Digest)] = data

	r := &StageSetReconciler{Client: c, RESTMapper: c.RESTMapper(), RollbackStore: store}
	applier := apply.New(c, c.RESTMapper(), stagesv1.GroupVersion.Group)
	// A fetcher that would fail (the producer is "gone"); it must not be reached.
	deadFetcher := &artifact.Fetcher{HTTPClient: http.DefaultClient, URLValidator: artifact.PermissiveHTTPURL, IPValidator: artifact.PermissiveIP}

	if reason, msg := r.attemptRollback(context.Background(), ss, applier, deadFetcher); reason != "" {
		t.Fatalf("store should make rollback succeed despite producer GC, got %q: %s", reason, msg)
	}
	if !cmExists(t, c, ns, "restored") {
		t.Fatal("rollback should have applied the object stored in the rollback store")
	}
}
