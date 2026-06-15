// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package preview

import (
	"context"
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/inventory"
	"github.com/metio/stageset-controller/internal/stageinv"
)

// PruneItem is an object the controller would delete on the next reconcile,
// paired with its live state so the diff can show exactly what goes away.
type PruneItem struct {
	Stage  string
	Ref    inventory.ObjectRef
	Object *unstructured.Unstructured // live object; nil if already gone from the cluster
}

// PrunePlan computes the deletions a reconcile would perform, using the same
// inventory diff the controller uses. rendered maps a rendered stage's name to
// the object refs it would now own. Stages absent from rendered (e.g. excluded
// by --stage) are held at their stored inventory so they are treated as
// unchanged — only genuinely-removed objects and removed stages are reported.
//
// A stage with prune disabled (spec.prune=false) is skipped; a stage removed
// from the spec is always torn down. Per-object prune opt-out
// (stages.metio.wtf/prune=disabled) is honored against the live object.
func (e *Engine) PrunePlan(ctx context.Context, ss *stagesv1.StageSet, rendered map[string][]inventory.ObjectRef) ([]PruneItem, error) {
	recorder := &stageinv.Recorder{Client: e.Client}
	stored, err := recorder.StageRecords(ctx, ss.Name, ss.Namespace)
	if err != nil {
		return nil, err
	}

	previous := make([]inventory.StageRecord, 0, len(stored))
	for name, rec := range stored {
		previous = append(previous, inventory.StageRecord{Name: name, Position: rec.Position, Entries: rec.Refs})
	}

	desired := make([]inventory.StageRecord, 0, len(ss.Spec.Stages))
	for i, st := range ss.Spec.Stages {
		switch {
		case rendered[st.Name] != nil:
			desired = append(desired, inventory.StageRecord{Name: st.Name, Position: i, Entries: rendered[st.Name]})
		case hasStage(stored, st.Name):
			desired = append(desired, inventory.StageRecord{Name: st.Name, Position: i, Entries: stored[st.Name].Refs})
		}
	}

	plan := inventory.ComputePlan(previous, desired)

	var items []PruneItem
	for stage, refs := range plan.PrunePerStage {
		if !pruneEnabled(ss, stage) {
			continue
		}
		items = append(items, e.fetchPruneItems(ctx, stage, refs)...)
	}
	for _, removed := range plan.RemovedStages {
		items = append(items, e.fetchPruneItems(ctx, removed.Name, removed.Entries)...)
	}

	sort.Slice(items, func(i, j int) bool {
		if items[i].Stage != items[j].Stage {
			return items[i].Stage < items[j].Stage
		}
		return items[i].Ref.ID() < items[j].Ref.ID()
	})
	return items, nil
}

// fetchPruneItems loads the live object behind each ref so the diff shows the
// resource as it stands. An object already gone (NotFound) or opted out via the
// prune annotation is dropped — nothing would be deleted.
func (e *Engine) fetchPruneItems(ctx context.Context, stage string, refs []inventory.ObjectRef) []PruneItem {
	var items []PruneItem
	for _, ref := range refs {
		live := &unstructured.Unstructured{}
		live.SetGroupVersionKind(schema.GroupVersionKind{Group: ref.Group, Version: ref.Version, Kind: ref.Kind})
		key := client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}
		if err := e.Client.Get(ctx, key, live); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			// A read error (RBAC, unknown kind) should not hide the intent: show
			// the deletion from the ref alone, without live detail.
			items = append(items, PruneItem{Stage: stage, Ref: ref})
			continue
		}
		if live.GetAnnotations()[stagesv1.PruneAnnotation] == "disabled" {
			continue
		}
		items = append(items, PruneItem{Stage: stage, Ref: ref, Object: live})
	}
	return items
}

func hasStage(stored map[string]stageinv.StageRecord, name string) bool {
	_, ok := stored[name]
	return ok
}

// pruneEnabled reports whether a surviving stage prunes (spec.prune, default
// true). A stage not present in the spec is governed by removed-stage teardown,
// not this toggle.
func pruneEnabled(ss *stagesv1.StageSet, stage string) bool {
	for _, st := range ss.Spec.Stages {
		if st.Name == stage {
			return st.Prune == nil || *st.Prune
		}
	}
	return true
}
