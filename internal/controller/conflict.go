// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
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
	forceEnabledValue      = "enabled"
	keepExistingAnnotation = "stages.metio.wtf/keep-existing"
	keepExistingValue      = "true"
)

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
func resolveConflictHandling(objects []*unstructured.Unstructured, stage *stagesv1.Stage) (apply.ConflictHandling, error) {
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
			setAnnotation(obj, forceAnnotation, forceEnabledValue)
			ch.ForceSelector = map[string]string{forceAnnotation: forceEnabledValue}
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
	if obj.GetAnnotations()[forceAnnotation] == forceEnabledValue {
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
