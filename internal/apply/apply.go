// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

// Package apply server-side-applies a stage's rendered objects and waits for
// them to become ready, wrapping fluxcd/pkg/ssa's ResourceManager with the
// field manager and owner labels this controller uses.
package apply

import (
	"context"
	"time"

	"github.com/fluxcd/cli-utils/pkg/kstatus/polling"
	"github.com/fluxcd/cli-utils/pkg/kstatus/status"
	"github.com/fluxcd/cli-utils/pkg/object"
	"github.com/fluxcd/pkg/ssa"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

// FieldManager is the server-side-apply field manager every StageSet write is
// attributed to.
const FieldManager = "stageset-controller"

// Applier applies and waits for a set of objects under a single owner identity.
type Applier struct {
	rm *ssa.ResourceManager
	// client and mapper are kept alongside rm so Stalled can re-read an
	// object's live status without going through a second poll session.
	client client.Client
	mapper meta.RESTMapper
}

// New builds an Applier. ownerGroup is the label prefix for the owner labels
// SSA stamps on every applied object (e.g. "stages.metio.wtf"), used for
// prune-by-label.
func New(c client.Client, mapper meta.RESTMapper, ownerGroup string) *Applier {
	poller := polling.NewStatusPoller(c, mapper, polling.Options{})
	rm := ssa.NewResourceManager(c, poller, ssa.Owner{Field: FieldManager, Group: ownerGroup})
	return &Applier{rm: rm, client: c, mapper: mapper}
}

// ConflictHandling selects, per object, how an immutable-field conflict is
// resolved, via ssa's annotation/label selectors. ForceSelector objects are
// deleted and recreated on conflict; IfNotPresentSelector objects are created
// when absent but never patched (KeepExisting). The empty value is plain apply:
// an immutable conflict surfaces as an error.
type ConflictHandling struct {
	ForceSelector        map[string]string
	IfNotPresentSelector map[string]string
}

// Apply stamps owner labels and server-side-applies the objects in dependency
// order (CRDs and namespaces first), returning the change set. Conflict
// handling is per object via the selectors in conflicts.
func (a *Applier) Apply(ctx context.Context, name, namespace string, objects []*unstructured.Unstructured, conflicts ConflictHandling) (*ssa.ChangeSet, error) {
	a.rm.SetOwnerLabels(objects, name, namespace)
	opts := ssa.DefaultApplyOptions()
	opts.ForceSelector = conflicts.ForceSelector
	opts.IfNotPresentSelector = conflicts.IfNotPresentSelector
	opts.WaitInterval = 2 * time.Second
	opts.WaitTimeout = time.Minute
	return a.rm.ApplyAllStaged(ctx, objects, opts)
}

// Wait blocks until every object in the set is reported ready by kstatus, or
// timeout elapses (failing fast on a terminal failure).
func (a *Applier) Wait(ctx context.Context, set object.ObjMetadataSet, timeout time.Duration) error {
	return a.rm.WaitForSetWithContext(ctx, set, ssa.WaitOptions{
		Interval: 2 * time.Second,
		Timeout:  timeout,
		FailFast: true,
	})
}

// Stalled reports whether any object in the set has reached a terminal kstatus
// Failed state. It answers the question a failed Wait leaves open: did the
// clock run out on objects that were still rolling out, or did the wait give up
// on one that never will become ready?
//
// Wait runs with FailFast, so it already aborts the moment an object reaches
// Failed — but that distinction reaches the caller only as a prefix on an
// unwrapped error string, which is not something to branch a rollback on. This
// recomputes the status from the live objects instead.
//
// An object that cannot be read (deleted underneath us, RBAC revoked mid-wait,
// a kind the mapper no longer resolves) is NOT counted as stalled. Callers use
// this to decide whether to undo a release, and a transient read failure is the
// last thing that should trigger one.
func (a *Applier) Stalled(ctx context.Context, set object.ObjMetadataSet) bool {
	for _, id := range set {
		mapping, err := a.mapper.RESTMapping(id.GroupKind)
		if err != nil {
			continue
		}
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(mapping.GroupVersionKind)
		if err := a.client.Get(ctx, client.ObjectKey{Namespace: id.Namespace, Name: id.Name}, u); err != nil {
			continue
		}
		res, err := status.Compute(u)
		if err != nil || res == nil {
			continue
		}
		if res.Status == status.FailedStatus {
			return true
		}
	}
	return false
}

// Delete removes the given objects (background propagation). Used to prune the
// objects that fell out of a stage's inventory and to tear a StageSet down.
//
// Inclusions is set to this StageSet's owner labels so ssa re-checks the live
// object and skips any object that no longer carries them — an object adopted or
// relabeled by another manager between reconciles is left untouched rather than
// deleted out from under its new owner.
//
// Exclusions honors the per-object prune opt-out: a live object annotated
// stages.metio.wtf/prune=disabled is left in place, matching the preview/prune
// path so `stageset diff` and the real teardown agree.
func (a *Applier) Delete(ctx context.Context, name, namespace string, objects []*unstructured.Unstructured) (*ssa.ChangeSet, error) {
	opts := ssa.DefaultDeleteOptions()
	opts.Inclusions = a.rm.GetOwnerLabels(name, namespace)
	opts.Exclusions = map[string]string{stagesv1.PruneAnnotation: "disabled"}
	return a.rm.DeleteAll(ctx, objects, opts)
}
