// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package actionplan

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/util/jsonpath"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

// VersionLabel is the field spec.version.fromObject reads when no fieldPath is
// set — the version travels inside the manifests regardless of source kind.
const VersionLabel = "app.kubernetes.io/version"

// VersionStageIndex resolves which stage's rendered output carries the version.
// A StageSet has one version all its stages converge on, so an empty stageRef
// defaults to the first stage. Returns -1 and an error when unresolvable. The
// error carries no domain prefix; callers add their own (the reconciler wraps it
// as a terminal invalid-version, a preview reports it plainly).
func VersionStageIndex(ss *stagesv1.StageSet, stageRef string) (int, error) {
	if stageRef == "" {
		if len(ss.Spec.Stages) == 0 {
			return -1, fmt.Errorf("spec.version.fromObject has no stage and the StageSet declares none")
		}
		return 0, nil
	}
	for i := range ss.Spec.Stages {
		if ss.Spec.Stages[i].Name == stageRef {
			return i, nil
		}
	}
	return -1, fmt.Errorf("spec.version.fromObject.stage %q is not a stage", stageRef)
}

// FindVersionObject returns the rendered object matching the ref's Kind and Name
// (and APIVersion when set), or nil.
func FindVersionObject(objects []*unstructured.Unstructured, ref *stagesv1.ObjectVersionRef) *unstructured.Unstructured {
	for _, o := range objects {
		if o.GetKind() != ref.Kind || o.GetName() != ref.Name {
			continue
		}
		if ref.APIVersion != "" && o.GetAPIVersion() != ref.APIVersion {
			continue
		}
		return o
	}
	return nil
}

// ExtractVersionField reads the version string from an object. An empty fieldPath
// reads the VersionLabel; otherwise fieldPath is a kubectl-style JSONPath that
// must resolve to the bare version string. Errors carry no domain prefix.
func ExtractVersionField(obj *unstructured.Unstructured, fieldPath string) (string, error) {
	if fieldPath == "" {
		val, found, err := unstructured.NestedString(obj.Object, "metadata", "labels", VersionLabel)
		if err != nil || !found {
			return "", fmt.Errorf("%s %q has no %s label; set spec.version.fromObject.fieldPath to read the version from a different field",
				obj.GetKind(), obj.GetName(), VersionLabel)
		}
		return val, nil
	}
	jp := jsonpath.New("version").AllowMissingKeys(false)
	if err := jp.Parse(fieldPath); err != nil {
		return "", fmt.Errorf("spec.version.fromObject.fieldPath %q is not valid JSONPath: %v", fieldPath, err)
	}
	var buf strings.Builder
	if err := jp.Execute(&buf, obj.Object); err != nil {
		return "", fmt.Errorf("evaluating fieldPath %q on %s %q: %v", fieldPath, obj.GetKind(), obj.GetName(), err)
	}
	return buf.String(), nil
}
