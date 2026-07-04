// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package preview

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/inventory"
	"github.com/metio/stageset-controller/internal/stageinv"
)

// cmRef returns the ObjectRef and a matching live ConfigMap for a core-group
// ConfigMap in namespace "ns".
func cmRef(name string) (inventory.ObjectRef, *corev1.ConfigMap) {
	ref := inventory.ObjectRef{Group: "", Kind: "ConfigMap", Namespace: "ns", Name: name, Version: "v1"}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: name}}
	return ref, cm
}

// pruneBool returns a *bool for the spec.prune field.
//
// seedInventory writes a stage's stored inventory via the Recorder so the shard
// CRs carry the exact labels and entry encoding the production path produces.
func seedInventory(t *testing.T, c client.Client, ss *stagesv1.StageSet, stage string, position int, refs ...inventory.ObjectRef) {
	t.Helper()
	rec := &stageinv.Recorder{Client: c}
	if err := rec.Write(context.Background(), ss, stage, position, refs); err != nil {
		t.Fatalf("seed inventory for stage %q: %v", stage, err)
	}
}

// TestPrunePlan_ObjectFellOutOfStage seeds two objects for a stage but renders
// only one; the dropped object is reported as a PruneItem carrying its live
// state.
func TestPrunePlan_ObjectFellOutOfStage(t *testing.T) {
	scheme := testScheme(t)
	keepRef, keepCM := cmRef("keep")
	dropRef, dropCM := cmRef("drop")

	ss := stageSet("s1")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(keepCM, dropCM).Build()
	seedInventory(t, c, ss, "s1", 0, keepRef, dropRef)

	engine := NewEngine(c, false)
	rendered := map[string][]inventory.ObjectRef{"s1": {keepRef}}

	items, err := engine.PrunePlan(context.Background(), ss, rendered)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("want 1 prune item, got %d: %+v", len(items), items)
	}
	if items[0].Ref.Name != "drop" {
		t.Fatalf("want drop pruned, got %q", items[0].Ref.Name)
	}
	if items[0].Object == nil || items[0].Object.GetName() != "drop" {
		t.Fatalf("want live object attached, got %+v", items[0].Object)
	}
}

// TestPrunePlan_PruneDisabledStageSkipped sets spec.prune=false on a surviving
// stage; the dropped object is not reported.
func TestPrunePlan_PruneDisabledStageSkipped(t *testing.T) {
	scheme := testScheme(t)
	keepRef, keepCM := cmRef("keep")
	dropRef, dropCM := cmRef("drop")

	ss := stageSet("s1")
	ss.Spec.Stages[0].Prune = new(false)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(keepCM, dropCM).Build()
	seedInventory(t, c, ss, "s1", 0, keepRef, dropRef)

	engine := NewEngine(c, false)
	rendered := map[string][]inventory.ObjectRef{"s1": {keepRef}}

	items, err := engine.PrunePlan(context.Background(), ss, rendered)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("prune=false stage must report nothing, got %+v", items)
	}
}

// TestPrunePlan_PerObjectAnnotationExcluded marks the dropped live object with
// the prune-disabled annotation; it is excluded from the plan.
func TestPrunePlan_PerObjectAnnotationExcluded(t *testing.T) {
	scheme := testScheme(t)
	keepRef, keepCM := cmRef("keep")
	dropRef, dropCM := cmRef("drop")
	dropCM.SetAnnotations(map[string]string{stagesv1.PruneAnnotation: "disabled"})

	ss := stageSet("s1")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(keepCM, dropCM).Build()
	seedInventory(t, c, ss, "s1", 0, keepRef, dropRef)

	engine := NewEngine(c, false)
	rendered := map[string][]inventory.ObjectRef{"s1": {keepRef}}

	items, err := engine.PrunePlan(context.Background(), ss, rendered)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("annotation-opted-out object must not be pruned, got %+v", items)
	}
}

// TestPrunePlan_PerObjectAnnotationExcludedMixedCase pins the opt-out to the
// same case-insensitive match the ssa teardown uses (utils.AnyInMetadata's
// EqualFold): a "Disabled" value must spare the object in the preview too, or
// `stageset diff` would show a deletion the real prune does not perform.
func TestPrunePlan_PerObjectAnnotationExcludedMixedCase(t *testing.T) {
	scheme := testScheme(t)
	keepRef, keepCM := cmRef("keep")
	dropRef, dropCM := cmRef("drop")
	dropCM.SetAnnotations(map[string]string{stagesv1.PruneAnnotation: "Disabled"})

	ss := stageSet("s1")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(keepCM, dropCM).Build()
	seedInventory(t, c, ss, "s1", 0, keepRef, dropRef)

	engine := NewEngine(c, false)
	rendered := map[string][]inventory.ObjectRef{"s1": {keepRef}}

	items, err := engine.PrunePlan(context.Background(), ss, rendered)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("mixed-case prune opt-out must not be pruned in preview, got %+v", items)
	}
}

// TestPrunePlan_AlreadyGoneObjectDropped seeds an object in the inventory whose
// live counterpart was never created; a NotFound read drops it from the plan.
func TestPrunePlan_AlreadyGoneObjectDropped(t *testing.T) {
	scheme := testScheme(t)
	keepRef, keepCM := cmRef("keep")
	dropRef, _ := cmRef("ghost") // never added to the client

	ss := stageSet("s1")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(keepCM).Build()
	seedInventory(t, c, ss, "s1", 0, keepRef, dropRef)

	engine := NewEngine(c, false)
	rendered := map[string][]inventory.ObjectRef{"s1": {keepRef}}

	items, err := engine.PrunePlan(context.Background(), ss, rendered)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("object already gone must be dropped, got %+v", items)
	}
}

// TestPrunePlan_RemovedStageTorndown stores a stage that no longer exists in
// the spec; all of its objects are reported for teardown regardless of the
// surviving-stage prune toggle.
func TestPrunePlan_RemovedStageTorndown(t *testing.T) {
	scheme := testScheme(t)
	liveRef, liveCM := cmRef("legacy")

	// Spec has only s1; inventory still records a removed stage "old".
	ss := stageSet("s1")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(liveCM).Build()
	seedInventory(t, c, ss, "old", 1, liveRef)

	engine := NewEngine(c, false)
	// s1 renders nothing of its own; "old" is absent from the spec entirely.
	rendered := map[string][]inventory.ObjectRef{}

	items, err := engine.PrunePlan(context.Background(), ss, rendered)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Stage != "old" || items[0].Ref.Name != "legacy" {
		t.Fatalf("removed stage must be torn down, got %+v", items)
	}
}

// TestPrunePlan_UnrenderedStageHeldAtInventory excludes a stage from rendered
// (e.g. --stage filter); its stored objects are held as unchanged and never
// pruned.
func TestPrunePlan_UnrenderedStageHeldAtInventory(t *testing.T) {
	scheme := testScheme(t)
	heldRef, heldCM := cmRef("held")

	ss := stageSet("s1", "s2")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(heldCM).Build()
	seedInventory(t, c, ss, "s2", 1, heldRef)

	engine := NewEngine(c, false)
	// Only s1 is rendered; s2 is filtered out but still in the spec.
	rendered := map[string][]inventory.ObjectRef{"s1": {}}

	items, err := engine.PrunePlan(context.Background(), ss, rendered)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("filtered stage must be held unchanged, got %+v", items)
	}
}

// TestPrunePlan_ReadErrorShowsRefWithoutLive forces a Get failure (unregistered
// kind) so the plan still surfaces the deletion intent from the ref alone with
// no live object attached.
func TestPrunePlan_ReadErrorShowsRefWithoutLive(t *testing.T) {
	scheme := testScheme(t)
	keepRef, keepCM := cmRef("keep")
	// A read error other than NotFound (here a synthetic Forbidden, mimicking an
	// RBAC denial) must not hide the deletion intent.
	dropRef, _ := cmRef("denied")

	ss := stageSet("s1")
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(keepCM)
	c := base.WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if key.Name == "denied" {
				return apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, key.Name, nil)
			}
			return cl.Get(ctx, key, obj, opts...)
		},
	}).Build()
	seedInventory(t, c, ss, "s1", 0, keepRef, dropRef)

	engine := NewEngine(c, false)
	rendered := map[string][]inventory.ObjectRef{"s1": {keepRef}}

	items, err := engine.PrunePlan(context.Background(), ss, rendered)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Ref.Name != "denied" {
		t.Fatalf("want the unreadable ref reported, got %+v", items)
	}
	if items[0].Object != nil {
		t.Fatalf("unreadable object must have no live detail, got %+v", items[0].Object)
	}
}

// TestPrunePlan_SortedByStageThenRef confirms the deterministic ordering across
// stages and object IDs.
func TestPrunePlan_SortedByStageThenRef(t *testing.T) {
	scheme := testScheme(t)
	aRef, aCM := cmRef("aaa")
	bRef, bCM := cmRef("bbb")

	ss := stageSet("zeta", "alpha")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(aCM, bCM).Build()
	seedInventory(t, c, ss, "zeta", 0, aRef)
	seedInventory(t, c, ss, "alpha", 1, bRef)

	engine := NewEngine(c, false)
	// Both stages render nothing, so both objects fall out.
	rendered := map[string][]inventory.ObjectRef{"zeta": {}, "alpha": {}}

	items, err := engine.PrunePlan(context.Background(), ss, rendered)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("want 2 items, got %d: %+v", len(items), items)
	}
	if items[0].Stage != "alpha" || items[1].Stage != "zeta" {
		t.Fatalf("items must be sorted by stage, got %q then %q", items[0].Stage, items[1].Stage)
	}
}

// TestPruneEnabled_Defaults exercises the helper directly: a nil toggle prunes,
// an explicit false does not, and a stage absent from the spec defaults to
// prune (its teardown is governed elsewhere).
func TestPruneEnabled_Defaults(t *testing.T) {
	ss := stageSet("on", "off")
	ss.Spec.Stages[1].Prune = new(false)

	if !pruneEnabled(ss, "on") {
		t.Error("nil prune toggle must default to true")
	}
	if pruneEnabled(ss, "off") {
		t.Error("explicit prune=false must disable")
	}
	if !pruneEnabled(ss, "absent") {
		t.Error("stage absent from spec must default to true")
	}
}

// TestHasStage covers the stored-stage membership helper.
func TestHasStage(t *testing.T) {
	stored := map[string]stageinv.StageRecord{"present": {}}
	if !hasStage(stored, "present") {
		t.Error("present stage must report true")
	}
	if hasStage(stored, "missing") {
		t.Error("missing stage must report false")
	}
}
