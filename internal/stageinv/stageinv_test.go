// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package stageinv

import (
	"context"
	"fmt"
	"sort"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/inventory"
)

func ref(name string) inventory.ObjectRef {
	return inventory.ObjectRef{Kind: "ConfigMap", Namespace: "ns", Name: name, Version: "v1"}
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("clientgo AddToScheme: %v", err)
	}
	if err := stagesv1.AddToScheme(s); err != nil {
		t.Fatalf("stagesv1 AddToScheme: %v", err)
	}
	return s
}

func newRecorder(t *testing.T, shardCap int, objs ...client.Object) *Recorder {
	t.Helper()
	s := testScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	return &Recorder{Client: c, ShardCap: shardCap}
}

func stageSet(name, namespace string) *stagesv1.StageSet {
	return &stagesv1.StageSet{ObjectMeta: metav1.ObjectMeta{
		Name:      name,
		Namespace: namespace,
		UID:       types.UID("uid-" + name),
	}}
}

// refs builds n distinct ConfigMap refs with stable, sortable names.
func refs(n int) []inventory.ObjectRef {
	out := make([]inventory.ObjectRef, 0, n)
	for i := range n {
		out = append(out, ref(fmt.Sprintf("cm-%03d", i)))
	}
	return out
}

func sortedNames(rs []inventory.ObjectRef) []string {
	names := make([]string, 0, len(rs))
	for _, r := range rs {
		names = append(names, r.Name)
	}
	sort.Strings(names)
	return names
}

func TestShardCap_ClampsToDefaultWhenUnset(t *testing.T) {
	t.Parallel()
	if got := (&Recorder{}).shardCap(); got != inventory.DefaultShardCap {
		t.Fatalf("shardCap() with ShardCap=0 = %d, want default %d", got, inventory.DefaultShardCap)
	}
	if got := (&Recorder{ShardCap: -7}).shardCap(); got != inventory.DefaultShardCap {
		t.Fatalf("shardCap() with negative ShardCap = %d, want default %d", got, inventory.DefaultShardCap)
	}
	if got := (&Recorder{ShardCap: 3}).shardCap(); got != 3 {
		t.Fatalf("shardCap() with ShardCap=3 = %d, want 3", got)
	}
}

// labelledConfigMap builds an applied ConfigMap carrying the owner + stage
// labels the applier stamps, so ReconstructFromCluster's selector finds it.
func labelledConfigMap(name, ssName, ssNamespace, stage string) *unstructured.Unstructured {
	group := stagesv1.GroupVersion.Group
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"})
	u.SetNamespace(ssNamespace)
	u.SetName(name)
	u.SetLabels(map[string]string{
		group + "/name":      ssName,
		group + "/namespace": ssNamespace,
		stagesv1.StageLabel:  stage,
	})
	return u
}

// ReconstructFromCluster must list applied objects through the supplied
// listClient (the target cluster), not the recorder's own client (the
// controller cluster). With spec.kubeConfig the applied objects only exist on
// the target, so a reconstruction against r.Client would silently recover
// nothing and the next prune would delete live objects.
func TestReconstructFromCluster_ListsViaTargetClient(t *testing.T) {
	t.Parallel()
	const (
		ssName = "app"
		ns     = "ns"
		stage  = "deploy"
	)
	s := testScheme(t)
	// Controller cluster (r.Client): holds no applied objects.
	controllerClient := fake.NewClientBuilder().WithScheme(s).Build()
	// Target cluster (listClient): holds the live applied ConfigMaps.
	applied := labelledConfigMap("live-cm", ssName, ns, stage)
	targetClient := fake.NewClientBuilder().WithScheme(s).WithObjects(applied).Build()

	r := &Recorder{Client: controllerClient}
	rendered := []*unstructured.Unstructured{labelledConfigMap("live-cm", ssName, ns, stage)}

	recovered, err := r.ReconstructFromCluster(context.Background(), targetClient, ssName, ns, stage, rendered)
	if err != nil {
		t.Fatalf("ReconstructFromCluster: %v", err)
	}
	if len(recovered) != 1 || recovered[0].Name != "live-cm" {
		t.Fatalf("recovered = %v, want the live-cm from the target cluster", recovered)
	}

	// The same call against the controller client (the bug) recovers nothing.
	none, err := r.ReconstructFromCluster(context.Background(), controllerClient, ssName, ns, stage, rendered)
	if err != nil {
		t.Fatalf("ReconstructFromCluster (controller client): %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("recovered %v from the controller cluster, want nothing (the objects live on the target)", none)
	}
}

func TestWrite_ShardsAtBoundaryAndRoundTrips(t *testing.T) {
	t.Parallel()
	r := newRecorder(t, 2) // cap of 2 → 5 refs span 3 shards (2,2,1)
	ss := stageSet("app", "ns")
	want := refs(5)
	if err := r.Write(context.Background(), ss, "deploy", 1, want); err != nil {
		t.Fatalf("Write: %v", err)
	}

	var list stagesv1.StageInventoryList
	if err := r.Client.List(context.Background(), &list); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list.Items) != 3 {
		t.Fatalf("expected 3 shards for 5 refs at cap 2, got %d", len(list.Items))
	}
	for i := range list.Items {
		owners := list.Items[i].GetOwnerReferences()
		if len(owners) != 1 || owners[0].Name != "app" {
			t.Fatalf("shard %s not owned by the StageSet: %#v", list.Items[i].Name, owners)
		}
		if list.Items[i].Spec.StagePosition != 1 {
			t.Errorf("shard %s position = %d, want 1", list.Items[i].Name, list.Items[i].Spec.StagePosition)
		}
	}

	stored, err := r.Stored(context.Background(), "app", "ns", "deploy")
	if err != nil {
		t.Fatalf("Stored: %v", err)
	}
	if got := sortedNames(stored); len(got) != 5 {
		t.Fatalf("round-tripped %d refs, want 5: %v", len(got), got)
	}
}

func TestWrite_ShrinkDeletesSurplusShards(t *testing.T) {
	t.Parallel()
	r := newRecorder(t, 2)
	ss := stageSet("app", "ns")

	if err := r.Write(context.Background(), ss, "deploy", 0, refs(5)); err != nil {
		t.Fatalf("initial Write: %v", err)
	}
	// Shrinking to 1 ref needs a single shard; the surplus must be deleted.
	if err := r.Write(context.Background(), ss, "deploy", 0, refs(1)); err != nil {
		t.Fatalf("shrinking Write: %v", err)
	}

	var list stagesv1.StageInventoryList
	if err := r.Client.List(context.Background(), &list); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("after shrink expected 1 shard, got %d", len(list.Items))
	}
	stored, err := r.Stored(context.Background(), "app", "ns", "deploy")
	if err != nil {
		t.Fatalf("Stored: %v", err)
	}
	if len(stored) != 1 {
		t.Fatalf("after shrink stored %d refs, want 1", len(stored))
	}
}

func TestStored_SkipsMalformedEntries(t *testing.T) {
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
		Spec: stagesv1.StageInventorySpec{Entries: []stagesv1.InventoryEntry{
			{ID: "ns_good_" + "_ConfigMap", V: "v1"}, // valid (core group → empty)
			{ID: "garbage", V: "v1"},                 // malformed → skipped
		}},
	}
	r := newRecorder(t, 0, si)
	stored, err := r.Stored(context.Background(), "app", "ns", "deploy")
	if err != nil {
		t.Fatalf("Stored: %v", err)
	}
	if len(stored) != 1 || stored[0].Name != "good" {
		t.Fatalf("Stored = %#v, want only the well-formed [good] entry", stored)
	}
}

func TestStageRecords_GroupsByStage(t *testing.T) {
	t.Parallel()
	r := newRecorder(t, 2)
	ss := stageSet("app", "ns")
	if err := r.Write(context.Background(), ss, "deploy", 0, refs(3)); err != nil {
		t.Fatalf("Write deploy: %v", err)
	}
	if err := r.Write(context.Background(), ss, "verify", 2, refs(1)); err != nil {
		t.Fatalf("Write verify: %v", err)
	}

	records, err := r.StageRecords(context.Background(), "app", "ns")
	if err != nil {
		t.Fatalf("StageRecords: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("StageRecords returned %d stages, want 2: %#v", len(records), records)
	}
	if rec := records["deploy"]; rec.Position != 0 || len(rec.Refs) != 3 {
		t.Errorf("deploy record = %+v, want position 0 with 3 refs", rec)
	}
	if rec := records["verify"]; rec.Position != 2 || len(rec.Refs) != 1 {
		t.Errorf("verify record = %+v, want position 2 with 1 ref", rec)
	}
}

func TestDeleteStageShards_RemovesOnlyTheStage(t *testing.T) {
	t.Parallel()
	r := newRecorder(t, 2)
	ss := stageSet("app", "ns")
	if err := r.Write(context.Background(), ss, "deploy", 0, refs(3)); err != nil {
		t.Fatalf("Write deploy: %v", err)
	}
	if err := r.Write(context.Background(), ss, "verify", 1, refs(1)); err != nil {
		t.Fatalf("Write verify: %v", err)
	}

	if err := r.DeleteStageShards(context.Background(), "ns", "app", "deploy"); err != nil {
		t.Fatalf("DeleteStageShards: %v", err)
	}

	gone, err := r.Stored(context.Background(), "app", "ns", "deploy")
	if err != nil {
		t.Fatalf("Stored deploy: %v", err)
	}
	if len(gone) != 0 {
		t.Fatalf("deploy shards survived deletion: %#v", gone)
	}
	kept, err := r.Stored(context.Background(), "app", "ns", "verify")
	if err != nil {
		t.Fatalf("Stored verify: %v", err)
	}
	if len(kept) != 1 {
		t.Fatalf("verify shards must be untouched, got %d refs", len(kept))
	}
}

func TestRefOf(t *testing.T) {
	t.Parallel()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"})
	u.SetNamespace("ns")
	u.SetName("web")
	got := RefOf(u)
	want := inventory.ObjectRef{Group: "apps", Kind: "Deployment", Namespace: "ns", Name: "web", Version: "v1"}
	if got != want {
		t.Fatalf("RefOf = %#v, want %#v", got, want)
	}
}

func TestDiff(t *testing.T) {
	t.Parallel()
	stored := []inventory.ObjectRef{ref("a"), ref("b"), ref("c")}
	current := []inventory.ObjectRef{ref("a"), ref("c")}
	pruned := Diff(stored, current)
	if len(pruned) != 1 || pruned[0].Name != "b" {
		t.Fatalf("Diff = %#v, want only [b]", pruned)
	}
}

func TestDiff_NothingRemoved(t *testing.T) {
	t.Parallel()
	refs := []inventory.ObjectRef{ref("a"), ref("b")}
	if got := Diff(refs, refs); len(got) != 0 {
		t.Fatalf("Diff of identical sets = %#v, want empty", got)
	}
}

func TestObjects(t *testing.T) {
	t.Parallel()
	objs := Objects([]inventory.ObjectRef{ref("x")})
	if len(objs) != 1 {
		t.Fatalf("want 1 object, got %d", len(objs))
	}
	if objs[0].GetName() != "x" || objs[0].GetKind() != "ConfigMap" || objs[0].GetNamespace() != "ns" {
		t.Fatalf("unexpected object: %#v", objs[0].Object)
	}
}
