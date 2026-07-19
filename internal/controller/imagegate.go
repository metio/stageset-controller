// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/imageverify"
)

// skipImageVerificationAnnotation is the break-glass: its value (a reason) bypasses
// the image-verification gate for one stage and emits a Warning event, so a
// deliberate override is recorded rather than silent. Fail-closed is the default.
const skipImageVerificationAnnotation = "stages.metio.wtf/skip-image-verification"

const eventReasonImageVerificationSkipped = "ImageVerificationSkipped"

// verifyStageImages is the image-verification gate: before a stage applies its
// rendered objects, it verifies every container image they reference against the
// cluster ImageVerificationPolicies and pins each verified image to its digest (so a
// tag can't be swapped before apply). A verification failure — or, under
// --require-image-verification, an image no policy governs — returns an error, so
// failStage holds the stage under ReasonImageUnverified before anything is applied.
func (r *StageSetReconciler) verifyStageImages(ctx context.Context, ss *stagesv1.StageSet, stage string, objects []*unstructured.Unstructured) error {
	if r.ImageVerifier == nil {
		return nil // gate disabled: no verifier wired
	}
	if reason := ss.Annotations[skipImageVerificationAnnotation]; reason != "" {
		r.event(ss, corev1.EventTypeWarning, eventReasonImageVerificationSkipped,
			fmt.Sprintf("image verification skipped for stage %q by break-glass annotation: %s", stage, reason))
		return nil
	}

	policies, err := r.listImagePolicies(ctx)
	if err != nil {
		return err
	}
	if len(policies) == 0 && !r.RequireImageVerification {
		return nil // nothing to enforce
	}

	pins := map[string]string{}
	for _, ref := range imageverify.ExtractImages(objects) {
		matched, skipped := imageverify.Match(policies, ref)
		if skipped {
			continue // an audited policy Skip exemption
		}
		if len(matched) == 0 {
			if r.RequireImageVerification {
				return fmt.Errorf("image %q matches no ImageVerificationPolicy", ref)
			}
			continue // ungoverned, and not deny-by-default
		}
		var digest string
		for i := range matched {
			p := &matched[i]
			d, verr := r.ImageVerifier.Verify(ctx, ref, p.Spec.Authorities, p.Spec.RequireAttestations)
			if verr != nil {
				return fmt.Errorf("image %q rejected by ImageVerificationPolicy %q: %w", ref, p.Name, verr)
			}
			digest = d
		}
		if digest != "" && digest != ref {
			pins[ref] = digest
		}
	}
	return imageverify.PinImages(objects, pins)
}

// listImagePolicies reads the cluster ImageVerificationPolicies uncached — they are
// few and change rarely, and an uncached read avoids a cluster-wide informer.
func (r *StageSetReconciler) listImagePolicies(ctx context.Context) ([]stagesv1.ImageVerificationPolicy, error) {
	var list stagesv1.ImageVerificationPolicyList
	if err := r.graphReader().List(ctx, &list); err != nil {
		return nil, err
	}
	return list.Items, nil
}
