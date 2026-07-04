// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package stageinv

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

// errClient wraps a fake client so List and Delete can be forced to fail,
// exercising the error-return paths that a healthy fake client never reaches.
func errClient(t *testing.T, funcs interceptor.Funcs, objs ...client.Object) client.Client {
	t.Helper()
	s := testScheme(t)
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).WithInterceptorFuncs(funcs).Build()
}

func TestStored_ListErrorPropagates(t *testing.T) {
	t.Parallel()
	c := errClient(t, interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("boom")
		},
	})
	r := &Recorder{Client: c}
	if _, err := r.Stored(context.Background(), "app", "ns", "deploy"); err == nil {
		t.Fatal("Stored with a failing List = nil error, want the wrapped list error")
	}
}

func TestStageRecords_ListErrorPropagates(t *testing.T) {
	t.Parallel()
	c := errClient(t, interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("boom")
		},
	})
	r := &Recorder{Client: c}
	if _, err := r.StageRecords(context.Background(), "app", "ns"); err == nil {
		t.Fatal("StageRecords with a failing List = nil error, want the wrapped list error")
	}
}

// A malformed stored entry drops out of a stage's reconstructed refs (and is
// counted) rather than failing the whole StageRecords read.
func TestStageRecords_SkipsMalformedEntries(t *testing.T) {
	t.Parallel()
	si := &stagesv1.StageInventory{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "app-deploy-00",
			Namespace: "ns",
			Labels: map[string]string{
				stagesv1.StageSetLabel: "app",
				stagesv1.StageLabel:    "deploy",
				stagesv1.ShardLabel:    "0",
			},
		},
		Spec: stagesv1.StageInventorySpec{
			StagePosition: 4,
			Entries: []stagesv1.InventoryEntry{
				{ID: "ns_good__ConfigMap", V: "v1"}, // valid (core group → empty)
				{ID: "garbage", V: "v1"},            // malformed → skipped
			},
		},
	}
	r := newRecorder(t, 0, si)
	records, err := r.StageRecords(context.Background(), "app", "ns")
	if err != nil {
		t.Fatalf("StageRecords: %v", err)
	}
	rec := records["deploy"]
	if rec.Position != 4 {
		t.Errorf("deploy position = %d, want 4", rec.Position)
	}
	if len(rec.Refs) != 1 || rec.Refs[0].Name != "good" {
		t.Fatalf("deploy refs = %#v, want only the well-formed [good] entry", rec.Refs)
	}
}

func TestWrite_CreateOrUpdateErrorPropagates(t *testing.T) {
	t.Parallel()
	c := errClient(t, interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return errors.New("boom")
		},
	})
	r := &Recorder{Client: c, ShardCap: 2}
	ss := stageSet("app", "ns")
	if err := r.Write(context.Background(), ss, "deploy", 0, refs(2)); err == nil {
		t.Fatal("Write with a failing Get inside CreateOrUpdate = nil error, want a write-shard error")
	}
}

func TestDeleteObsoleteShards_ListErrorPropagates(t *testing.T) {
	t.Parallel()
	c := errClient(t, interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("boom")
		},
	})
	r := &Recorder{Client: c, ShardCap: 2}
	if err := r.deleteObsoleteShards(context.Background(), "ns", "app", "deploy", 0); err == nil {
		t.Fatal("deleteObsoleteShards with a failing List = nil error, want the wrapped list error")
	}
}

func TestDeleteObsoleteShards_DeleteErrorPropagates(t *testing.T) {
	t.Parallel()
	// A surplus shard (index >= keep) present in the store, with Delete forced to
	// fail with a non-NotFound error, drives the delete-error branch.
	surplus := &stagesv1.StageInventory{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "app-deploy-05",
			Namespace: "ns",
			Labels: map[string]string{
				stagesv1.StageSetLabel: "app",
				stagesv1.StageLabel:    "deploy",
				stagesv1.ShardLabel:    "5",
			},
		},
	}
	c := errClient(t, interceptor.Funcs{
		Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
			return errors.New("boom")
		},
	}, surplus)
	r := &Recorder{Client: c, ShardCap: 2}
	if err := r.deleteObsoleteShards(context.Background(), "ns", "app", "deploy", 0); err == nil {
		t.Fatal("deleteObsoleteShards with a failing Delete = nil error, want the wrapped delete error")
	}
}

// A shard whose index label is unparsable is skipped rather than deleted, so a
// keep of 0 leaves it untouched.
func TestDeleteObsoleteShards_SkipsUnparsableShardIndex(t *testing.T) {
	t.Parallel()
	weird := &stagesv1.StageInventory{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "app-deploy-x",
			Namespace: "ns",
			Labels: map[string]string{
				stagesv1.StageSetLabel: "app",
				stagesv1.StageLabel:    "deploy",
				stagesv1.ShardLabel:    "not-a-number",
			},
		},
	}
	r := newRecorder(t, 2, weird)
	if err := r.deleteObsoleteShards(context.Background(), "ns", "app", "deploy", 0); err != nil {
		t.Fatalf("deleteObsoleteShards: %v", err)
	}
	var list stagesv1.StageInventoryList
	if err := r.Client.List(context.Background(), &list); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("the unparsable-index shard was deleted; want it skipped, items=%d", len(list.Items))
	}
}

func TestDeleteStageShards_ListErrorPropagates(t *testing.T) {
	t.Parallel()
	c := errClient(t, interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("boom")
		},
	})
	r := &Recorder{Client: c}
	if err := r.DeleteStageShards(context.Background(), "ns", "app", "deploy"); err == nil {
		t.Fatal("DeleteStageShards with a failing List = nil error, want the wrapped list error")
	}
}

func TestDeleteStageShards_DeleteErrorPropagates(t *testing.T) {
	t.Parallel()
	shard := &stagesv1.StageInventory{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "app-deploy-00",
			Namespace: "ns",
			Labels: map[string]string{
				stagesv1.StageSetLabel: "app",
				stagesv1.StageLabel:    "deploy",
				stagesv1.ShardLabel:    "0",
			},
		},
	}
	c := errClient(t, interceptor.Funcs{
		Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
			return errors.New("boom")
		},
	}, shard)
	r := &Recorder{Client: c}
	if err := r.DeleteStageShards(context.Background(), "ns", "app", "deploy"); err == nil {
		t.Fatal("DeleteStageShards with a failing Delete = nil error, want the wrapped delete error")
	}
}

// Per-GVK list failures are aggregated into the returned error while the refs
// that did list are still returned.
func TestReconstructFromCluster_AggregatesListErrors(t *testing.T) {
	t.Parallel()
	listClient := errClient(t, interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("boom")
		},
	})
	r := &Recorder{Client: errClient(t, interceptor.Funcs{})}
	rendered := []*unstructured.Unstructured{labelledConfigMap("cm", "app", "ns", "deploy")}
	refs, err := r.ReconstructFromCluster(context.Background(), listClient, "app", "ns", "deploy", rendered)
	if err == nil {
		t.Fatal("ReconstructFromCluster with a failing List = nil error, want the aggregated list error")
	}
	if len(refs) != 0 {
		t.Fatalf("refs = %#v, want none (every list failed)", refs)
	}
}

// Reconstruction over multiple GVKs lists each kind through the target client
// and dedupes the discovered refs by ID.
func TestReconstructFromCluster_MultipleGVKs(t *testing.T) {
	t.Parallel()
	s := testScheme(t)
	cm := labelledConfigMap("cm", "app", "ns", "deploy")
	secret := &unstructured.Unstructured{}
	secret.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "Secret"})
	secret.SetNamespace("ns")
	secret.SetName("sec")
	group := stagesv1.GroupVersion.Group
	secret.SetLabels(map[string]string{
		group + "/name":      "app",
		group + "/namespace": "ns",
		stagesv1.StageLabel:  "deploy",
	})
	target := fake.NewClientBuilder().WithScheme(s).WithObjects(cm, secret).Build()

	r := &Recorder{Client: fake.NewClientBuilder().WithScheme(s).Build()}
	rendered := []*unstructured.Unstructured{
		labelledConfigMap("cm", "app", "ns", "deploy"),
		secret.DeepCopy(),
	}
	refs, err := r.ReconstructFromCluster(context.Background(), target, "app", "ns", "deploy", rendered)
	if err != nil {
		t.Fatalf("ReconstructFromCluster: %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("recovered %d refs, want 2 (a ConfigMap and a Secret): %#v", len(refs), refs)
	}
}
