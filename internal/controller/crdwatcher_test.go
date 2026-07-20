/*
 * SPDX-FileCopyrightText: The stageset-controller Authors
 * SPDX-License-Identifier: 0BSD
 */

package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	toolscache "k8s.io/client-go/tools/cache"
)

var gitRepoGVK = schema.GroupVersionKind{Group: "source.toolkit.fluxcd.io", Version: "v1", Kind: "GitRepository"}

// establishedCRD builds an Established CRD serving the given versions.
func establishedCRD(name, group, kind string, servedVersions ...string) *apiextv1.CustomResourceDefinition {
	vers := make([]apiextv1.CustomResourceDefinitionVersion, 0, len(servedVersions))
	for _, v := range servedVersions {
		vers = append(vers, apiextv1.CustomResourceDefinitionVersion{Name: v, Served: true})
	}
	return &apiextv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: apiextv1.CustomResourceDefinitionSpec{
			Group:    group,
			Names:    apiextv1.CustomResourceDefinitionNames{Kind: kind},
			Versions: vers,
		},
		Status: apiextv1.CustomResourceDefinitionStatus{
			Conditions: []apiextv1.CustomResourceDefinitionCondition{
				{Type: apiextv1.Established, Status: apiextv1.ConditionTrue},
			},
		},
	}
}

// recordingEngager records EngageProducerWatch calls and reports a fixed
// missing set, so the CRD→engage handoff can be asserted without an informer.
type recordingEngager struct {
	missing []schema.GroupVersionKind
	calls   []schema.GroupVersionKind
	err     error
}

func (r *recordingEngager) MissingProducerKinds() []schema.GroupVersionKind { return r.missing }

func (r *recordingEngager) EngageProducerWatch(_ context.Context, gvk schema.GroupVersionKind) error {
	r.calls = append(r.calls, gvk)
	return r.err
}

func TestCRDServesGVK(t *testing.T) {
	cases := []struct {
		name string
		crd  *apiextv1.CustomResourceDefinition
		gvk  schema.GroupVersionKind
		want bool
	}{
		{
			name: "established, matching group/kind/served-version",
			crd:  establishedCRD("gitrepositories.source.toolkit.fluxcd.io", "source.toolkit.fluxcd.io", "GitRepository", "v1"),
			gvk:  gitRepoGVK,
			want: true,
		},
		{
			name: "not established",
			crd: func() *apiextv1.CustomResourceDefinition {
				crd := establishedCRD("gitrepositories.source.toolkit.fluxcd.io", "source.toolkit.fluxcd.io", "GitRepository", "v1")
				crd.Status.Conditions[0].Status = apiextv1.ConditionFalse
				return crd
			}(),
			gvk:  gitRepoGVK,
			want: false,
		},
		{
			name: "wrong group",
			crd:  establishedCRD("gitrepositories.other.example.com", "other.example.com", "GitRepository", "v1"),
			gvk:  gitRepoGVK,
			want: false,
		},
		{
			name: "wrong kind",
			crd:  establishedCRD("buckets.source.toolkit.fluxcd.io", "source.toolkit.fluxcd.io", "Bucket", "v1"),
			gvk:  gitRepoGVK,
			want: false,
		},
		{
			name: "version served but not the one requested",
			crd:  establishedCRD("gitrepositories.source.toolkit.fluxcd.io", "source.toolkit.fluxcd.io", "GitRepository", "v1beta2"),
			gvk:  gitRepoGVK,
			want: false,
		},
		{
			name: "requested version present but not served",
			crd: func() *apiextv1.CustomResourceDefinition {
				crd := establishedCRD("gitrepositories.source.toolkit.fluxcd.io", "source.toolkit.fluxcd.io", "GitRepository")
				crd.Spec.Versions = []apiextv1.CustomResourceDefinitionVersion{{Name: "v1", Served: false}}
				return crd
			}(),
			gvk:  gitRepoGVK,
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := crdServesGVK(tc.crd, tc.gvk); got != tc.want {
				t.Errorf("crdServesGVK = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIsCRDEstablished(t *testing.T) {
	t.Run("no conditions", func(t *testing.T) {
		if isCRDEstablished(&apiextv1.CustomResourceDefinition{}) {
			t.Error("empty conditions reported Established")
		}
	})
	t.Run("only NamesAccepted", func(t *testing.T) {
		crd := &apiextv1.CustomResourceDefinition{Status: apiextv1.CustomResourceDefinitionStatus{
			Conditions: []apiextv1.CustomResourceDefinitionCondition{{Type: apiextv1.NamesAccepted, Status: apiextv1.ConditionTrue}},
		}}
		if isCRDEstablished(crd) {
			t.Error("NamesAccepted-only reported Established")
		}
	})
	t.Run("Established=True", func(t *testing.T) {
		if !isCRDEstablished(establishedCRD("x.example.com", "example.com", "X", "v1")) {
			t.Error("Established=True misread")
		}
	})
}

func TestHandleCRD_EngagesMatchingMissingKind(t *testing.T) {
	rec := &recordingEngager{missing: []schema.GroupVersionKind{gitRepoGVK}}
	w := &CRDWatcher{Engager: rec}
	w.handleCRD(context.Background(), establishedCRD("gitrepositories.source.toolkit.fluxcd.io", "source.toolkit.fluxcd.io", "GitRepository", "v1"))
	if len(rec.calls) != 1 || rec.calls[0] != gitRepoGVK {
		t.Errorf("EngageProducerWatch calls = %v, want [%v]", rec.calls, gitRepoGVK)
	}
}

func TestHandleCRD_IgnoresNotEstablished(t *testing.T) {
	rec := &recordingEngager{missing: []schema.GroupVersionKind{gitRepoGVK}}
	w := &CRDWatcher{Engager: rec}
	crd := establishedCRD("gitrepositories.source.toolkit.fluxcd.io", "source.toolkit.fluxcd.io", "GitRepository", "v1")
	crd.Status.Conditions[0].Status = apiextv1.ConditionFalse
	w.handleCRD(context.Background(), crd)
	if len(rec.calls) != 0 {
		t.Errorf("engaged on a not-yet-Established CRD: %v", rec.calls)
	}
}

func TestHandleCRD_IgnoresKindNotMissing(t *testing.T) {
	// A StageSet references only GitRepository; a Bucket CRD landing must not
	// engage a watch nobody asked for — this is the precision the reference-
	// driven missing set buys over a fixed kind list.
	rec := &recordingEngager{missing: []schema.GroupVersionKind{gitRepoGVK}}
	w := &CRDWatcher{Engager: rec}
	w.handleCRD(context.Background(), establishedCRD("buckets.source.toolkit.fluxcd.io", "source.toolkit.fluxcd.io", "Bucket", "v1"))
	if len(rec.calls) != 0 {
		t.Errorf("engaged an unreferenced kind: %v", rec.calls)
	}
}

func TestHandleCRD_IgnoresNonCRDObject(t *testing.T) {
	rec := &recordingEngager{missing: []schema.GroupVersionKind{gitRepoGVK}}
	w := &CRDWatcher{Engager: rec}
	w.handleCRD(context.Background(), &metav1.PartialObjectMetadata{})
	if len(rec.calls) != 0 {
		t.Errorf("engaged on a non-CRD object: %v", rec.calls)
	}
}

func TestHandleCRD_EngageErrorDoesNotPanic(t *testing.T) {
	rec := &recordingEngager{missing: []schema.GroupVersionKind{gitRepoGVK}, err: errors.New("watch failed")}
	w := &CRDWatcher{Engager: rec}
	// The reconcile path is the backstop; a failed engage is logged, not fatal.
	w.handleCRD(context.Background(), establishedCRD("gitrepositories.source.toolkit.fluxcd.io", "source.toolkit.fluxcd.io", "GitRepository", "v1"))
	if len(rec.calls) != 1 {
		t.Errorf("expected one engage attempt, got %d", len(rec.calls))
	}
}

func TestCRDWatcher_RejectsNilEngager(t *testing.T) {
	w := &CRDWatcher{RestCfg: &rest.Config{Host: "http://127.0.0.1:1"}}
	if err := w.Start(context.Background()); err == nil {
		t.Fatal("expected error for nil engager")
	}
}

// TestCRDWatcher_CacheSyncFailureDegradesNotFatal pins that a cache-sync failure
// (e.g. missing customresourcedefinitions RBAC before the chart catches up) does
// NOT propagate out of Start — that would take down the whole manager, including
// the reconcilers that are working fine. Start must log and return nil;
// engagement falls back to the reconcile path until the process restarts.
func TestCRDWatcher_CacheSyncFailureDegradesNotFatal(t *testing.T) {
	orig := waitForCacheSync
	t.Cleanup(func() { waitForCacheSync = orig })
	waitForCacheSync = func(_ <-chan struct{}, _ ...toolscache.InformerSynced) bool { return false }

	w := &CRDWatcher{
		RestCfg: &rest.Config{Host: "http://127.0.0.1:1"},
		Engager: &recordingEngager{},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := w.Start(ctx); err != nil {
		t.Fatalf("Start returned %v on cache-sync failure, want nil (degrade, not crash)", err)
	}
}
