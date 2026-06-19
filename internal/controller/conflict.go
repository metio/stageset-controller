// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/apply"
)

// Conflict-handling marker annotations. ssa matches these on the applied
// objects to decide per-object force/keep behavior; they persist on the
// in-cluster object the way kustomize-controller's force annotation does.
const (
	forceAnnotation        = "stages.metio.wtf/force"
	keepExistingAnnotation = "stages.metio.wtf/keep-existing"
	keepExistingValue      = "true"
)

// newForceToken mints a fresh per-apply force token. It is a package-level seam
// so tests can substitute a deterministic value; see resolveConflictHandling for
// why the value must be unique per apply.
var newForceToken = func() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand never fails on supported platforms; fall back to a fixed
		// non-empty value rather than an empty selector that matches nothing.
		return "enabled"
	}
	return hex.EncodeToString(b[:])
}

// Conflict-policy action values, matching the CRD enum.
const (
	conflictFail         = "Fail"
	conflictRecreate     = "Recreate"
	conflictKeepExisting = "KeepExisting"
)

// resolveConflictHandling decides, per object, how an immutable-field conflict
// is handled and returns the ssa selectors that realize it. Precedence per
// object: a `stages.metio.wtf/force: enabled` annotation (explicit user opt-in)
// wins; then the first matching conflictPolicy rule; then the effective default
// (conflictPolicy.default, or Recreate when stage.force is set, else Fail).
//
// It stamps marker annotations on the objects and refuses to recreate a
// PersistentVolumeClaim / PersistentVolume from a *rule* unless that rule sets
// allowDataLoss — recreating those destroys data, so it must be said out loud.
// The blunt stage.force and the explicit per-object annotation are treated as
// the operator already having opted in, and are not gated.
// forceToken is the per-apply value stamped on this-pass Recreate objects and
// used as the ForceSelector value; the caller mints a fresh one per apply (via
// newForceToken). It MUST be unique per apply: ssa's shouldForceApply matches
// ForceSelector against the live object too, so a stale force annotation left on
// an in-cluster object from an earlier Recreate pass would match a constant
// selector and force-recreate an object whose current policy is Fail (PVC/PV
// data loss). A fresh token per apply means a stale annotation carries an older
// value that can never match the current selector.
func resolveConflictHandling(objects []*unstructured.Unstructured, stage *stagesv1.Stage, forceToken string) (apply.ConflictHandling, error) {
	effectiveDefault := conflictFail
	if stage.Force {
		effectiveDefault = conflictRecreate
	}
	policy := stage.ConflictPolicy
	if policy != nil && policy.Default != "" {
		effectiveDefault = policy.Default
	}

	var ch apply.ConflictHandling
	for _, obj := range objects {
		action, allowDataLoss, fromRule := conflictActionFor(obj, policy, effectiveDefault)
		switch action {
		case conflictRecreate:
			if fromRule && !allowDataLoss && isStatefulData(obj) {
				return apply.ConflictHandling{}, fmt.Errorf(
					"conflictPolicy refuses to recreate %s %q: that destroys data; set the rule's allowDataLoss: true to permit it",
					obj.GetKind(), obj.GetName(),
				)
			}
			setAnnotation(obj, forceAnnotation, forceToken)
			ch.ForceSelector = map[string]string{forceAnnotation: forceToken}
		case conflictKeepExisting:
			setAnnotation(obj, keepExistingAnnotation, keepExistingValue)
			ch.IfNotPresentSelector = map[string]string{keepExistingAnnotation: keepExistingValue}
		}
	}
	return ch, nil
}

// conflictActionFor returns one object's conflict action, whether data loss is
// permitted, and whether the action came from a rule (which gates the PV/PVC
// recreate guard).
func conflictActionFor(obj *unstructured.Unstructured, policy *stagesv1.ConflictPolicy, fallback string) (action string, allowDataLoss, fromRule bool) {
	// Any non-empty force annotation on the desired object is an explicit
	// operator opt-in to recreate. The value carried on the desired object is
	// authoritative for this pass; resolveConflictHandling re-stamps it with the
	// per-apply token so the selector matches.
	if obj.GetAnnotations()[forceAnnotation] != "" {
		return conflictRecreate, false, false
	}
	if policy != nil {
		for i := range policy.Rules {
			if matchesConflictTarget(obj, &policy.Rules[i].Target) {
				return policy.Rules[i].Action, policy.Rules[i].AllowDataLoss, true
			}
		}
	}
	return fallback, false, false
}

// matchesConflictTarget reports whether obj matches a rule target; an unset
// target field matches any value.
func matchesConflictTarget(obj *unstructured.Unstructured, t *stagesv1.ConflictTarget) bool {
	if t.APIVersion != "" && t.APIVersion != obj.GetAPIVersion() {
		return false
	}
	if t.Kind != "" && t.Kind != obj.GetKind() {
		return false
	}
	if t.Name != "" && t.Name != obj.GetName() {
		return false
	}
	if t.Namespace != "" && t.Namespace != obj.GetNamespace() {
		return false
	}
	return true
}

// isStatefulData reports whether obj is a core/v1 PersistentVolumeClaim or
// PersistentVolume — the kinds whose recreation destroys data.
func isStatefulData(obj *unstructured.Unstructured) bool {
	if obj.GetAPIVersion() != "v1" {
		return false
	}
	switch obj.GetKind() {
	case "PersistentVolumeClaim", "PersistentVolume":
		return true
	default:
		return false
	}
}

func setAnnotation(obj *unstructured.Unstructured, key, value string) {
	ann := obj.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	ann[key] = value
	obj.SetAnnotations(ann)
}
