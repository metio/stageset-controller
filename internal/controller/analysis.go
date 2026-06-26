// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"strconv"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/apply"
	"github.com/metio/stageset-controller/internal/artifact"
	"github.com/metio/stageset-controller/internal/inventory"
	"github.com/metio/stageset-controller/internal/metrics"
	"github.com/metio/stageset-controller/internal/metricsource"
	"github.com/metio/stageset-controller/internal/stageinv"
)

// analysisVerdict is one promotion-analysis evaluation: the per-check results,
// whether any check's value breached its threshold, and whether any source was
// unreadable. gatePromotion turns it into a phase + failure count.
type analysisVerdict struct {
	result    *stagesv1.AnalysisResult
	breached  bool
	sourceErr bool
}

// evaluatePromotionAnalysis queries every check of a stage's promotion analysis
// once and reports the verdict. A check whose source can't be read counts as a
// source error (routed by onSourceError); a readable value outside its threshold
// counts as a breach. It does not decide the hold — gatePromotion does, applying
// failureLimit and the dryRun/onFailure/onSourceError policy.
func (r *StageSetReconciler) evaluatePromotionAnalysis(ctx context.Context, ss *stagesv1.StageSet, stage *stagesv1.Stage) analysisVerdict {
	an := stage.Promotion.Analysis
	res := &stagesv1.AnalysisResult{Time: &metav1.Time{Time: r.now()}}
	v := analysisVerdict{result: res}
	allOK := true
	for i := range an.Checks {
		c := &an.Checks[i]
		cr := stagesv1.AnalysisCheckResult{Name: c.Name}
		value, err := r.MetricQuerier.Query(ctx, ss.Namespace, c.Source)
		switch {
		case err != nil:
			metrics.MetricSourceErrorsTotal.WithLabelValues(ss.Namespace, ss.Name).Inc()
			cr.Error = err.Error()
			v.sourceErr = true
			allOK = false
		default:
			cr.Value = strconv.FormatFloat(value, 'f', -1, 64)
			ok, terr := metricsource.ThresholdSatisfied(c.Threshold, value)
			if terr != nil {
				// A malformed threshold normally fails admission; treat the
				// fallback as a breach so a bad rule never silently promotes.
				cr.Error = terr.Error()
				v.breached = true
				allOK = false
			} else {
				cr.OK = ok
				if !ok {
					v.breached = true
					allOK = false
				}
			}
		}
		res.Checks = append(res.Checks, cr)
	}
	res.Passed = allOK
	return v
}

// analysisInterval is the re-evaluation cadence while an analysis holds.
func (r *StageSetReconciler) analysisInterval(ss *stagesv1.StageSet, an *stagesv1.PromotionAnalysis) time.Duration {
	if an != nil && an.Interval != nil && an.Interval.Duration > 0 {
		return an.Interval.Duration
	}
	return r.retryInterval(ss)
}

// rollbackAborted reports whether a stage is parked reverted by a prior
// onFailure=Rollback for the currently pinned revision — so the stage loop skips
// re-applying (and re-failing) that revision. A fresh manual promote token
// clears the abort (handled by not skipping, so the gate's break-glass fires).
func rollbackAborted(ss *stagesv1.StageSet, stage *stagesv1.Stage, prior stagesv1.StageStatus, revision string) bool {
	p := stage.Promotion
	if p == nil || p.Analysis == nil || p.Analysis.OnFailure != "Rollback" || p.Analysis.DryRun {
		return false
	}
	st := prior.PromotionState
	if st == nil || st.Phase != stagesv1.PromotionBlocked || st.AbortedRevision != revision {
		return false
	}
	if tok := promoteTokenFor(ss, stage.Name); tok != "" && tok != prior.LastHandledPromotion {
		return false // a fresh promote un-aborts the stage
	}
	return true
}

// rollbackStageToSnapshot reverts a single stage to its last-good revision from
// status.lastAppliedSnapshot, reusing the rollback render+apply helper. It is
// scoped to this stage only — earlier promoted stages are untouched. A missing
// snapshot (rollbackOnFailure never recorded one) returns ok=false so the caller
// falls back to holding the stage instead. The returned revision is the
// last-good one now live, for the stage status.
func (r *StageSetReconciler) rollbackStageToSnapshot(ctx context.Context, ss *stagesv1.StageSet, stage *stagesv1.Stage, position int, applier *apply.Applier, fetcher *artifact.Fetcher, recorder *stageinv.Recorder) (revision string, ok bool, err error) {
	var ref *stagesv1.StageArtifactRef
	for i := range ss.Status.LastAppliedSnapshot {
		if ss.Status.LastAppliedSnapshot[i].Stage == stage.Name {
			ref = &ss.Status.LastAppliedSnapshot[i]
			break
		}
	}
	if ref == nil {
		return "", false, nil
	}
	dec, derr := r.buildDecryptor(ctx, ss)
	if derr != nil {
		return "", false, derr
	}
	objects, rbReason, _, rbErr := r.rollbackStageObjects(ctx, ss, stage, *ref, fetcher, dec)
	if rbErr != nil {
		return "", false, rbErr
	}
	if rbReason != "" {
		// The snapshot's revision is no longer fetchable / reproducible. Can't
		// revert; the caller holds instead.
		return "", false, nil
	}
	apply.StampStageLabel(objects, stagesv1.StageLabel, stage.Name)
	if _, aerr := applier.Apply(ctx, ss.Name, ss.Namespace, objects, apply.ConflictHandling{}); aerr != nil {
		return "", false, aerr
	}
	refs := make([]inventory.ObjectRef, 0, len(objects))
	for _, o := range objects {
		refs = append(refs, stageinv.RefOf(o))
	}
	if werr := recorder.Write(ctx, ss, stage.Name, position, refs); werr != nil {
		return "", false, werr
	}
	return ref.Revision, true, nil
}
