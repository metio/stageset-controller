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
	"github.com/fluxcd/cli-utils/pkg/object"
	"github.com/fluxcd/pkg/ssa"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// FieldManager is the server-side-apply field manager every StageSet write is
// attributed to.
const FieldManager = "stageset-controller"

// Applier applies and waits for a set of objects under a single owner identity.
type Applier struct {
	rm *ssa.ResourceManager
}

// New builds an Applier. ownerGroup is the label prefix for the owner labels
// SSA stamps on every applied object (e.g. "stages.metio.wtf"), used for
// prune-by-label.
func New(c client.Client, mapper meta.RESTMapper, ownerGroup string) *Applier {
	poller := polling.NewStatusPoller(c, mapper, polling.Options{})
	rm := ssa.NewResourceManager(c, poller, ssa.Owner{Field: FieldManager, Group: ownerGroup})
	return &Applier{rm: rm}
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

// Delete removes the given objects (background propagation). Used to prune the
// objects that fell out of a stage's inventory and to tear a StageSet down.
//
// Inclusions is set to this StageSet's owner labels so ssa re-checks the live
// object and skips any object that no longer carries them — an object adopted or
// relabeled by another manager between reconciles is left untouched rather than
// deleted out from under its new owner.
func (a *Applier) Delete(ctx context.Context, name, namespace string, objects []*unstructured.Unstructured) (*ssa.ChangeSet, error) {
	opts := ssa.DefaultDeleteOptions()
	opts.Inclusions = a.rm.GetOwnerLabels(name, namespace)
	return a.rm.DeleteAll(ctx, objects, opts)
}
