// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

// Package stageinv persists a stage's applied-object inventory as sharded
// StageInventory CRs and diffs the stored inventory against a fresh apply to
// find prune candidates. The pure planning logic (IDs, sharding) lives in
// internal/inventory; this package is the Kubernetes-client layer over it.
package stageinv

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/inventory"
	"github.com/metio/stageset-controller/internal/metrics"
)

// Recorder reads and writes a stage's StageInventory shards.
type Recorder struct {
	Client   client.Client
	ShardCap int
}

func (r *Recorder) shardCap() int {
	if r.ShardCap > 0 {
		return r.ShardCap
	}
	return inventory.DefaultShardCap
}

// ReconstructFromCluster best-effort rebuilds a stage's object references from
// the live cluster, for the disaster case where the StageInventory was lost
// (a stray delete, a partial restore) while its applied objects still exist. It
// lists objects still carrying the owner labels the applier stamps
// (<group>/name, <group>/namespace) and the per-stage discovery label, across
// the GVKs present in the current render.
//
// The label values are case-exact matches: object names and namespaces are
// lowercase by Kubernetes (RFC 1123) and stage names are lowercase by the
// StageSet CRD's `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$` validation, so the values
// stamped and the values queried here are identical without any normalisation.
//
// Reconstruction is best-effort: a GVK no longer in the render cannot be
// discovered this way, and per-GVK list errors are aggregated and returned
// while the refs that were listed are still returned for the caller to use.
//
// listClient is the cluster the applied objects live on — the target cluster
// when spec.kubeConfig retargets the apply, else the controller's own client.
// It is separate from r.Client (which reads/writes the StageInventory shards on
// the controller cluster), because a remote apply's objects are never visible
// to r.Client.
func (r *Recorder) ReconstructFromCluster(ctx context.Context, listClient client.Client, ssName, ssNamespace, stage string, rendered []*unstructured.Unstructured) ([]inventory.ObjectRef, error) {
	group := stagesv1.GroupVersion.Group
	sel := client.MatchingLabels{
		group + "/name":      ssName,
		group + "/namespace": ssNamespace,
		stagesv1.StageLabel:  stage,
	}
	gvks := map[schema.GroupVersionKind]struct{}{}
	for _, o := range rendered {
		gvks[o.GroupVersionKind()] = struct{}{}
	}
	seen := map[string]inventory.ObjectRef{}
	var errs []error
	for gvk := range gvks {
		var list unstructured.UnstructuredList
		list.SetGroupVersionKind(schema.GroupVersionKind{Group: gvk.Group, Version: gvk.Version, Kind: gvk.Kind + "List"})
		if err := listClient.List(ctx, &list, sel); err != nil {
			errs = append(errs, fmt.Errorf("list %s: %w", gvk, err))
			continue
		}
		for i := range list.Items {
			ref := RefOf(&list.Items[i])
			seen[ref.ID()] = ref
		}
	}
	refs := make([]inventory.ObjectRef, 0, len(seen))
	for _, ref := range seen {
		refs = append(refs, ref)
	}
	return refs, errors.Join(errs...)
}

// Stored returns the object references currently recorded for a stage across
// all of its shards (malformed entries are skipped).
func (r *Recorder) Stored(ctx context.Context, ssName, namespace, stage string) ([]inventory.ObjectRef, error) {
	var list stagesv1.StageInventoryList
	if err := r.Client.List(ctx, &list, client.InNamespace(namespace), client.MatchingLabels{
		stagesv1.StageSetLabel: ssName,
		stagesv1.StageLabel:    stage,
	}); err != nil {
		return nil, fmt.Errorf("list StageInventory: %w", err)
	}
	var refs []inventory.ObjectRef
	for i := range list.Items {
		for _, e := range list.Items[i].Spec.Entries {
			ref, err := inventory.ParseID(e.ID, e.V)
			if err != nil {
				skippedEntry(ctx, namespace, ssName, stage, e.ID, err)
				continue
			}
			refs = append(refs, ref)
		}
	}
	return refs, nil
}

// skippedEntry records a malformed inventory entry: a debug log line with the
// offending ID and a counter bump. A skipped entry means the object it named
// drops out of the stored set and so escapes pruning, so the signal must not be
// silent.
func skippedEntry(ctx context.Context, namespace, stageset, stage, id string, err error) {
	log.FromContext(ctx).V(1).Info("skipping malformed StageInventory entry",
		"namespace", namespace, "stageset", stageset, "stage", stage, "id", id, "error", err)
	metrics.InventorySkippedEntriesTotal.WithLabelValues(namespace, stageset, stage).Inc()
}

// Write replaces the stored shards for a stage with refs, owned by the
// StageSet, and removes any surplus shards left by a larger previous run.
func (r *Recorder) Write(ctx context.Context, ss *stagesv1.StageSet, stage string, position int, refs []inventory.ObjectRef) error {
	shards, err := inventory.PlanShards(refs, r.shardCap())
	if err != nil {
		return fmt.Errorf("plan shards: %w", err)
	}
	for i, shard := range shards {
		si := &stagesv1.StageInventory{ObjectMeta: metav1.ObjectMeta{
			Name:      inventory.ShardName(ss.Name, stage, i),
			Namespace: ss.Namespace,
		}}
		shardIndex := i
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, si, func() error {
			si.Labels = map[string]string{
				stagesv1.StageSetLabel: ss.Name,
				stagesv1.StageLabel:    stage,
				stagesv1.ShardLabel:    strconv.Itoa(shardIndex),
			}
			// #nosec G115 -- a stage position is bounded by spec.stages (small).
			si.Spec.StagePosition = int32(position)
			si.Spec.Entries = entriesOf(shard)
			return controllerutil.SetControllerReference(ss, si, r.Client.Scheme())
		}); err != nil {
			return fmt.Errorf("write shard %s: %w", si.Name, err)
		}
	}
	return r.deleteObsoleteShards(ctx, ss.Namespace, ss.Name, stage, len(shards))
}

// deleteObsoleteShards removes a stage's shards that the just-written set has
// superseded: those whose index is >= keep (shrinking a stage's object set
// drops the no-longer-needed shards), and those whose name is not the canonical
// ShardName for their index. The latter self-migrates any shard written under an
// older, non-injective naming scheme — Write has already created the canonical
// shard for that index, so the stale-named one is redundant. Both are found by
// the stage's label selector, and the canonical shards just written match their
// own names, so only genuinely obsolete objects are removed.
func (r *Recorder) deleteObsoleteShards(ctx context.Context, namespace, ssName, stage string, keep int) error {
	var list stagesv1.StageInventoryList
	if err := r.Client.List(ctx, &list, client.InNamespace(namespace), client.MatchingLabels{
		stagesv1.StageSetLabel: ssName,
		stagesv1.StageLabel:    stage,
	}); err != nil {
		return fmt.Errorf("list StageInventory for cleanup: %w", err)
	}
	for i := range list.Items {
		idx, err := strconv.Atoi(list.Items[i].Labels[stagesv1.ShardLabel])
		if err != nil {
			continue
		}
		if idx < keep && list.Items[i].Name == inventory.ShardName(ssName, stage, idx) {
			continue
		}
		if err := r.Client.Delete(ctx, &list.Items[i]); err != nil && client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete obsolete shard %s: %w", list.Items[i].Name, err)
		}
	}
	return nil
}

func entriesOf(shard []inventory.ObjectRef) []stagesv1.InventoryEntry {
	entries := make([]stagesv1.InventoryEntry, 0, len(shard))
	for _, ref := range shard {
		entries = append(entries, stagesv1.InventoryEntry{ID: ref.ID(), V: ref.Version})
	}
	return entries
}

// StageRecord is a stored stage's recorded position and object references.
type StageRecord struct {
	Position int
	Refs     []inventory.ObjectRef
}

// StageRecords groups all stored inventory of a StageSet by stage name (used
// for teardown of removed stages and of the whole StageSet on deletion).
func (r *Recorder) StageRecords(ctx context.Context, ssName, namespace string) (map[string]StageRecord, error) {
	var list stagesv1.StageInventoryList
	if err := r.Client.List(ctx, &list, client.InNamespace(namespace), client.MatchingLabels{
		stagesv1.StageSetLabel: ssName,
	}); err != nil {
		return nil, fmt.Errorf("list StageInventory: %w", err)
	}
	records := map[string]StageRecord{}
	for i := range list.Items {
		item := &list.Items[i]
		stage := item.Labels[stagesv1.StageLabel]
		rec := records[stage]
		rec.Position = int(item.Spec.StagePosition)
		for _, e := range item.Spec.Entries {
			ref, err := inventory.ParseID(e.ID, e.V)
			if err != nil {
				skippedEntry(ctx, namespace, ssName, stage, e.ID, err)
				continue
			}
			rec.Refs = append(rec.Refs, ref)
		}
		records[stage] = rec
	}
	return records, nil
}

// DeleteStageShards removes all StageInventory shards of a stage (used when a
// stage is removed from the spec; on StageSet deletion the owner reference GCs
// them instead).
func (r *Recorder) DeleteStageShards(ctx context.Context, namespace, ssName, stage string) error {
	var list stagesv1.StageInventoryList
	if err := r.Client.List(ctx, &list, client.InNamespace(namespace), client.MatchingLabels{
		stagesv1.StageSetLabel: ssName,
		stagesv1.StageLabel:    stage,
	}); err != nil {
		return fmt.Errorf("list StageInventory: %w", err)
	}
	for i := range list.Items {
		if err := r.Client.Delete(ctx, &list.Items[i]); err != nil && client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete shard %s: %w", list.Items[i].Name, err)
		}
	}
	return nil
}

// Diff returns the refs present in stored but not in current — the objects
// that fell out of a stage and are prune candidates.
func Diff(stored, current []inventory.ObjectRef) []inventory.ObjectRef {
	keep := make(map[string]struct{}, len(current))
	for _, ref := range current {
		keep[ref.ID()] = struct{}{}
	}
	var pruned []inventory.ObjectRef
	for _, ref := range stored {
		if _, ok := keep[ref.ID()]; !ok {
			pruned = append(pruned, ref)
		}
	}
	return pruned
}

// RefOf builds an ObjectRef from an applied object.
func RefOf(o *unstructured.Unstructured) inventory.ObjectRef {
	gvk := o.GroupVersionKind()
	return inventory.ObjectRef{
		Group:     gvk.Group,
		Kind:      gvk.Kind,
		Namespace: o.GetNamespace(),
		Name:      o.GetName(),
		Version:   gvk.Version,
	}
}

// Objects builds the minimal unstructured objects (GVK + name/namespace) for a
// set of refs, suitable for a delete request.
func Objects(refs []inventory.ObjectRef) []*unstructured.Unstructured {
	out := make([]*unstructured.Unstructured, 0, len(refs))
	for _, ref := range refs {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(schema.GroupVersionKind{Group: ref.Group, Version: ref.Version, Kind: ref.Kind})
		u.SetNamespace(ref.Namespace)
		u.SetName(ref.Name)
		out = append(out, u)
	}
	return out
}
