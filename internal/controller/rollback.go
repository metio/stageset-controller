// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/apply"
	"github.com/metio/stageset-controller/internal/artifact"
	"github.com/metio/stageset-controller/internal/build"
	"github.com/metio/stageset-controller/internal/decryptor"
)

// substitutionDigest fingerprints a stage's resolved postBuild substitution map
// so rollback can tell whether the inputs are still the ones that produced the
// last good apply, WITHOUT storing the (possibly secret) values themselves. An
// empty map yields the empty string, which disables the rollback check (nothing
// to verify). Keys are sorted and each pair is length-prefixed so distinct maps
// can't collide on the concatenation.
func substitutionDigest(vars map[string]string) string {
	if len(vars) == 0 {
		return ""
	}
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		fmt.Fprintf(h, "%d:%s=%d:%s\n", len(k), k, len(vars[k]), vars[k])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// RollbackStore is the opt-in external store (e.g. S3/OCI) for the rendered
// output of successful runs. It makes rollback bit-exact and independent of
// producer retention: the controller pushes each stage's rendered objects on
// success and pulls them back on rollback, instead of re-fetching the producer
// artifact. Nil disables it (rollback falls back to re-fetch + re-render). The
// interface is bytes-only so backends stay free of the Kubernetes API.
type RollbackStore interface {
	Put(ctx context.Context, key string, data []byte) error
	Get(ctx context.Context, key string) (data []byte, found bool, err error)
}

// rollbackKey addresses a stage's rendered output by artifact digest, so the
// same content de-duplicates and the key is stable across reconciles.
func rollbackKey(ss *stagesv1.StageSet, stage, digest string) string {
	return fmt.Sprintf("%s/%s/%s/%s", ss.Namespace, ss.Name, stage, strings.ReplaceAll(digest, ":", "-"))
}

func encodeObjects(objects []*unstructured.Unstructured) ([]byte, error) {
	raw := make([]map[string]any, len(objects))
	for i, o := range objects {
		raw[i] = o.Object
	}
	return json.Marshal(raw)
}

func decodeObjects(data []byte) ([]*unstructured.Unstructured, error) {
	var raw []map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	out := make([]*unstructured.Unstructured, len(raw))
	for i, m := range raw {
		out[i] = &unstructured.Unstructured{Object: m}
	}
	return out, nil
}

// storeRendered pushes a stage's rendered output to the external store on a
// successful apply. A store failure is evented, not fatal — the apply already
// succeeded, and rollback can still fall back to re-fetch.
func (r *StageSetReconciler) storeRendered(ctx context.Context, ss *stagesv1.StageSet, stage, digest string, objects []*unstructured.Unstructured) {
	data, err := encodeObjects(objects)
	if err != nil {
		r.event(ss, corev1.EventTypeWarning, "RollbackStoreFailed", fmt.Sprintf("encoding stage %q for the rollback store failed: %v", stage, err))
		return
	}
	if err := r.RollbackStore.Put(ctx, rollbackKey(ss, stage, digest), data); err != nil {
		r.event(ss, corev1.EventTypeWarning, "RollbackStoreFailed", fmt.Sprintf("storing stage %q in the rollback store failed: %v", stage, err))
	}
}

// snapshotStages records the per-stage artifact pointers of a successful run.
// It stores only coordinates (URL + digest + revision) — a pointer to the
// producer's immutable, revision-addressed content — never the rendered output,
// so status stays small (no Helm-style release-size limit) and carries no
// substituteFrom secret values.
// subDigests carries the per-stage substitution fingerprint (aligned by stage
// index) of the same successful run, so rollback can detect a substituteFrom
// source that changed in the rollback window.
func snapshotStages(ss *stagesv1.StageSet, resolved []artifact.ResolvedArtifact, subDigests []string) []stagesv1.StageArtifactRef {
	out := make([]stagesv1.StageArtifactRef, 0, len(ss.Spec.Stages))
	for i := range ss.Spec.Stages {
		ra := resolved[i]
		out = append(out, stagesv1.StageArtifactRef{
			Stage: ss.Spec.Stages[i].Name, URL: ra.URL, Digest: ra.Digest, Revision: ra.Revision,
			SubstitutionDigest: subDigests[i],
		})
	}
	return out
}

// attemptRollback restores the last-good revisions recorded in
// status.lastAppliedSnapshot: re-fetch each (digest-verified), re-render under
// the CURRENT spec (its path, patches, and a live re-read of postBuild
// substitution), and re-apply in forward order. Pruning needs no special
// handling — converging back to old content is an ordinary inventory diff. A
// revision the producer has garbage-collected makes rollback terminal
// PreviousRevisionUnavailable; an empty reason means restored (or there was
// nothing to restore). When an external RollbackStore holds the run, its
// bit-exact copy is used instead of re-fetching.
func (r *StageSetReconciler) attemptRollback(ctx context.Context, ss *stagesv1.StageSet, applier *apply.Applier, fetcher *artifact.Fetcher) (reason, msg string) {
	snap := ss.Status.LastAppliedSnapshot
	if len(snap) == 0 {
		return "", "" // no snapshot (e.g. the first run failed): nothing to roll back to
	}
	// Re-fetch rollback rebuilds the source, so it must decrypt the same way the
	// forward path does. The store path holds already-rendered objects and skips
	// this.
	dec, derr := r.buildDecryptor(ctx, ss)
	if derr != nil {
		return ReasonPreviousRevisionUnavailable, fmt.Sprintf("cannot roll back: configuring decryption failed (%v)", derr)
	}
	stages := make(map[string]*stagesv1.Stage, len(ss.Spec.Stages))
	for i := range ss.Spec.Stages {
		stages[ss.Spec.Stages[i].Name] = &ss.Spec.Stages[i]
	}
	for _, ref := range snap {
		stage, ok := stages[ref.Stage]
		if !ok {
			continue // stage removed from the spec: not restored, pruned normally
		}
		objects, rbReason, rbMsg := r.rollbackStageObjects(ctx, ss, stage, ref, fetcher, dec)
		if rbReason != "" {
			return rbReason, rbMsg
		}
		if _, aerr := applier.Apply(ctx, ss.Name, ss.Namespace, objects, apply.ConflictHandling{}); aerr != nil {
			return ReasonPreviousRevisionUnavailable,
				fmt.Sprintf("cannot roll back stage %q: re-applying the previous revision failed (%v)", ref.Stage, aerr)
		}
	}
	return "", ""
}

// rollbackStageObjects re-fetches the recorded revision (digest-verified) and
// re-renders it under the current spec.
func (r *StageSetReconciler) rollbackStageObjects(ctx context.Context, ss *stagesv1.StageSet, stage *stagesv1.Stage, ref stagesv1.StageArtifactRef, fetcher *artifact.Fetcher, dec *decryptor.Decryptor) ([]*unstructured.Unstructured, string, string) {
	// Bit-exact, GC-independent path: the external store holds the rendered
	// output from when this revision was last applied.
	if r.RollbackStore != nil {
		if data, found, err := r.RollbackStore.Get(ctx, rollbackKey(ss, ref.Stage, ref.Digest)); err == nil && found {
			if objects, derr := decodeObjects(data); derr == nil {
				return objects, "", ""
			}
		}
		// store miss/error falls through to producer re-fetch
	}
	files, ferr := fetcher.Fetch(ctx, ref.URL, ref.Digest, "")
	if ferr != nil {
		return nil, ReasonPreviousRevisionUnavailable,
			fmt.Sprintf("cannot roll back: revision %s for stage %q is no longer fetchable (%v)", ref.Revision, ref.Stage, ferr)
	}
	files, ferr = decryptFiles(dec, files)
	if ferr != nil {
		return nil, ReasonPreviousRevisionUnavailable,
			fmt.Sprintf("cannot roll back stage %q: decrypting the previous revision failed (%v)", ref.Stage, ferr)
	}
	vars, verr := r.resolvePostBuildVars(ctx, ss.Namespace, stage.PostBuild)
	if verr != nil {
		return nil, ReasonPreviousRevisionUnavailable,
			fmt.Sprintf("cannot roll back stage %q: resolving postBuild variables failed (%v)", ref.Stage, verr)
	}
	// Faithful-or-fail: if the substitution inputs changed since the recorded
	// run, re-rendering the old artifact would NOT reproduce the previous state.
	// Refuse rather than silently apply a different result. An empty snapshot
	// digest (pre-upgrade snapshot, or no substitution) skips the check.
	if ref.SubstitutionDigest != "" && substitutionDigest(vars) != ref.SubstitutionDigest {
		return nil, ReasonPreviousRevisionUnavailable,
			fmt.Sprintf("cannot roll back stage %q: its postBuild substitution inputs changed since the last good apply, so the previous rendered state can no longer be reproduced — fix forward instead", ref.Stage)
	}
	objects, berr := build.Build(files, build.Options{Path: stage.Path, Patches: stage.Patches}, vars)
	if berr != nil {
		return nil, ReasonPreviousRevisionUnavailable,
			fmt.Sprintf("cannot roll back stage %q: rebuilding the previous revision failed (%v)", ref.Stage, berr)
	}
	return objects, "", ""
}
