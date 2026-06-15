// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package diffrender

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// noiseFields are server-populated or controller-managed metadata fields that
// carry no authored intent. Stripping them keeps rendered output and diffs
// stable and free of churn an operator did not cause.
var noiseMetadataFields = []string{
	"managedFields",
	"resourceVersion",
	"uid",
	"generation",
	"creationTimestamp",
	"selfLink",
}

// StripNoise removes server-populated metadata, the kubectl last-applied
// annotation, and the entire status subtree from a copy-safe object in place.
func StripNoise(obj *unstructured.Unstructured) {
	if obj == nil {
		return
	}
	for _, f := range noiseMetadataFields {
		unstructured.RemoveNestedField(obj.Object, "metadata", f)
	}
	unstructured.RemoveNestedField(obj.Object, "metadata", "annotations",
		"kubectl.kubernetes.io/last-applied-configuration")
	unstructured.RemoveNestedField(obj.Object, "status")

	// Drop an annotations map left empty by the removal above, so it does not
	// render as `annotations: {}`.
	if ann, found, _ := unstructured.NestedMap(obj.Object, "metadata", "annotations"); found && len(ann) == 0 {
		unstructured.RemoveNestedField(obj.Object, "metadata", "annotations")
	}
}

// ToYAML encodes a single object to YAML.
func ToYAML(obj *unstructured.Unstructured) ([]byte, error) {
	return yaml.Marshal(obj.Object)
}

// RenderManifests masks and YAML-encodes a list of objects into one multi-doc
// stream, document-separated by "---". The masker is applied first so Secret
// values never reach the writer.
func RenderManifests(objs []*unstructured.Unstructured, masker *SecretMasker) (string, error) {
	var b []byte
	for i, obj := range objs {
		c := obj.DeepCopy()
		masker.Mask(c)
		out, err := ToYAML(c)
		if err != nil {
			return "", err
		}
		if i > 0 {
			b = append(b, []byte("---\n")...)
		}
		b = append(b, out...)
	}
	return string(b), nil
}
