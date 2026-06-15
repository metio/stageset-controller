// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package diffrender

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func secret(data map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata":   map[string]any{"name": "creds"},
		"data":       data,
	}}
}

func TestMask_EqualValuesShareIndex(t *testing.T) {
	s := secret(map[string]any{"a": "QQ==", "b": "QQ==", "c": "Qg=="})
	NewSecretMasker(false).Mask(s)

	data, _, _ := unstructured.NestedStringMap(s.Object, "data")
	if data["a"] != data["b"] {
		t.Errorf("equal Secret values masked differently: %q vs %q", data["a"], data["b"])
	}
	if data["a"] == data["c"] {
		t.Errorf("distinct Secret values share a mask: %q", data["a"])
	}
	for k, v := range data {
		if !strings.HasPrefix(v, "<-- value not shown") {
			t.Errorf("data[%s] not masked: %q", k, v)
		}
		if v == "QQ==" || v == "Qg==" {
			t.Errorf("plaintext leaked for %s: %q", k, v)
		}
	}
}

func TestMask_StringData(t *testing.T) {
	s := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata":   map[string]any{"name": "creds"},
		"stringData": map[string]any{"token": "hunter2"},
	}}
	NewSecretMasker(false).Mask(s)
	sd, _, _ := unstructured.NestedStringMap(s.Object, "stringData")
	if strings.Contains(sd["token"], "hunter2") {
		t.Errorf("stringData plaintext leaked: %q", sd["token"])
	}
}

func TestMask_RevealIsNoop(t *testing.T) {
	s := secret(map[string]any{"a": "QQ=="})
	NewSecretMasker(true).Mask(s)
	data, _, _ := unstructured.NestedStringMap(s.Object, "data")
	if data["a"] != "QQ==" {
		t.Errorf("reveal masker altered value: %q", data["a"])
	}
}

func TestMask_NonSecretUntouched(t *testing.T) {
	cm := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": "cfg"},
		"data":     map[string]any{"key": "value"},
	}}
	NewSecretMasker(false).Mask(cm)
	data, _, _ := unstructured.NestedStringMap(cm.Object, "data")
	if data["key"] != "value" {
		t.Errorf("ConfigMap value masked: %q", data["key"])
	}
}

func TestStripNoise_RemovesServerFields(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{
			"name":              "web",
			"resourceVersion":   "123",
			"uid":               "abc",
			"generation":        int64(4),
			"creationTimestamp": "2026-01-01T00:00:00Z",
			"managedFields":     []any{map[string]any{"manager": "x"}},
			"annotations": map[string]any{
				"kubectl.kubernetes.io/last-applied-configuration": "{...}",
				"keep": "this",
			},
		},
		"spec":   map[string]any{"replicas": int64(3)},
		"status": map[string]any{"readyReplicas": int64(3)},
	}}
	StripNoise(obj)

	md, _, _ := unstructured.NestedMap(obj.Object, "metadata")
	for _, gone := range []string{"resourceVersion", "uid", "generation", "creationTimestamp", "managedFields"} {
		if _, present := md[gone]; present {
			t.Errorf("metadata.%s not stripped", gone)
		}
	}
	if _, present := obj.Object["status"]; present {
		t.Error("status not stripped")
	}
	ann, _, _ := unstructured.NestedStringMap(obj.Object, "metadata", "annotations")
	if _, present := ann["kubectl.kubernetes.io/last-applied-configuration"]; present {
		t.Error("last-applied annotation not stripped")
	}
	if ann["keep"] != "this" {
		t.Error("real annotation wrongly removed")
	}
}

func TestStripNoise_DropsEmptyAnnotations(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Service",
		"metadata": map[string]any{
			"name":        "svc",
			"annotations": map[string]any{"kubectl.kubernetes.io/last-applied-configuration": "{}"},
		},
	}}
	StripNoise(obj)
	if _, found, _ := unstructured.NestedMap(obj.Object, "metadata", "annotations"); found {
		t.Error("emptied annotations map should be removed")
	}
}

func TestRenderManifests_MasksAndSeparates(t *testing.T) {
	objs := []*unstructured.Unstructured{
		{Object: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "a"}}},
		secret(map[string]any{"p": "c2VjcmV0"}),
	}
	out, err := RenderManifests(objs, NewSecretMasker(false))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "---") {
		t.Errorf("multi-doc separator missing:\n%s", out)
	}
	if strings.Contains(out, "c2VjcmV0") {
		t.Errorf("secret plaintext leaked:\n%s", out)
	}
	if !strings.Contains(out, "value not shown") {
		t.Errorf("mask placeholder missing:\n%s", out)
	}
}
