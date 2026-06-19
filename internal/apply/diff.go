// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package apply

import (
	"context"
	"fmt"

	"github.com/fluxcd/pkg/ssa"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// DiffAction is the change a dry-run apply predicts for one object.
type DiffAction string

const (
	// DiffCreate: the object does not exist and would be created.
	DiffCreate DiffAction = "create"
	// DiffConfigure: the object exists and the merge would change it.
	DiffConfigure DiffAction = "configure"
	// DiffUnchanged: the merge produces no change.
	DiffUnchanged DiffAction = "unchanged"
	// DiffSkipped: the object is excluded from apply (e.g. KeepExisting).
	DiffSkipped DiffAction = "skip"
)

// DiffEntry is the predicted change for a single object. Existing is the live
// object (nil for a create); Merged is the object as it would be after apply
// (nil for unchanged/skip).
type DiffEntry struct {
	Action    DiffAction
	GVK       schema.GroupVersionKind
	Namespace string
	Name      string
	Existing  *unstructured.Unstructured
	Merged    *unstructured.Unstructured
}

// Diff server-side dry-run applies each object with the controller's field
// manager and reports what would change, without persisting anything. Owner
// labels are stamped first so the preview reflects exactly what a real apply
// would write, conflict selectors included.
func (a *Applier) Diff(ctx context.Context, name, namespace string, objects []*unstructured.Unstructured, conflicts ConflictHandling) ([]DiffEntry, error) {
	a.rm.SetOwnerLabels(objects, name, namespace)
	opts := ssa.DefaultDiffOptions()
	opts.ForceSelector = conflicts.ForceSelector
	opts.IfNotPresentSelector = conflicts.IfNotPresentSelector

	out := make([]DiffEntry, 0, len(objects))
	for _, obj := range objects {
		cse, existing, merged, err := a.rm.Diff(ctx, obj, opts)
		if err != nil {
			return nil, fmt.Errorf("diff %s %s/%s: %w",
				obj.GroupVersionKind(), obj.GetNamespace(), obj.GetName(), err)
		}
		e := DiffEntry{
			GVK:       obj.GroupVersionKind(),
			Namespace: obj.GetNamespace(),
			Name:      obj.GetName(),
		}
		switch cse.Action {
		case ssa.CreatedAction:
			e.Action = DiffCreate
			e.Merged = obj.DeepCopy()
		case ssa.ConfiguredAction:
			e.Action = DiffConfigure
			e.Existing = existing
			e.Merged = merged
		case ssa.UnchangedAction:
			e.Action = DiffUnchanged
		default:
			e.Action = DiffSkipped
		}
		out = append(out, e)
	}
	return out, nil
}
