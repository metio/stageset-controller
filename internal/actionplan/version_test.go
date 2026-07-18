// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package actionplan

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

func obj(apiVersion, kind, name string, set func(o *unstructured.Unstructured)) *unstructured.Unstructured {
	o := &unstructured.Unstructured{Object: map[string]any{}}
	o.SetAPIVersion(apiVersion)
	o.SetKind(kind)
	o.SetName(name)
	if set != nil {
		set(o)
	}
	return o
}

func TestFindVersionObject(t *testing.T) {
	objects := []*unstructured.Unstructured{
		obj("v1", "ConfigMap", "cfg", nil),
		obj("apps/v1", "Deployment", "web", nil),
		obj("apps/v1", "Deployment", "worker", nil),
	}
	tests := []struct {
		name     string
		ref      stagesv1.ObjectVersionRef
		wantName string
		wantNil  bool
	}{
		{name: "by kind and name", ref: stagesv1.ObjectVersionRef{Kind: "Deployment", Name: "web"}, wantName: "web"},
		{name: "apiVersion narrows", ref: stagesv1.ObjectVersionRef{Kind: "Deployment", Name: "web", APIVersion: "apps/v1"}, wantName: "web"},
		{name: "wrong apiVersion no match", ref: stagesv1.ObjectVersionRef{Kind: "Deployment", Name: "web", APIVersion: "v2"}, wantNil: true},
		{name: "unknown name no match", ref: stagesv1.ObjectVersionRef{Kind: "Deployment", Name: "absent"}, wantNil: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FindVersionObject(objects, &tt.ref)
			if tt.wantNil {
				if got != nil {
					t.Fatalf("expected nil, got %s", got.GetName())
				}
				return
			}
			if got == nil || got.GetName() != tt.wantName {
				t.Fatalf("got %v, want name %q", got, tt.wantName)
			}
		})
	}
}

func TestExtractVersionField(t *testing.T) {
	labeled := obj("apps/v1", "Deployment", "web", func(o *unstructured.Unstructured) {
		o.SetLabels(map[string]string{VersionLabel: "2.1.0"})
	})

	t.Run("default reads the version label", func(t *testing.T) {
		got, err := ExtractVersionField(labeled, "")
		if err != nil || got != "2.1.0" {
			t.Fatalf("got (%q, %v), want 2.1.0", got, err)
		}
	})
	t.Run("missing label names the label", func(t *testing.T) {
		_, err := ExtractVersionField(obj("v1", "ConfigMap", "c", nil), "")
		if err == nil || !strings.Contains(err.Error(), VersionLabel) {
			t.Fatalf("expected an error naming the label, got %v", err)
		}
	})
	t.Run("jsonpath fieldPath reads an arbitrary field", func(t *testing.T) {
		got, err := ExtractVersionField(labeled, "{.metadata.labels.app\\.kubernetes\\.io/version}")
		if err != nil || got != "2.1.0" {
			t.Fatalf("got (%q, %v), want 2.1.0", got, err)
		}
	})
	t.Run("invalid jsonpath errors", func(t *testing.T) {
		if _, err := ExtractVersionField(labeled, "{.unterminated"); err == nil {
			t.Fatal("expected a parse error for malformed JSONPath")
		}
	})
}

func TestVersionStageIndex(t *testing.T) {
	ss := &stagesv1.StageSet{}
	ss.Spec.Stages = []stagesv1.Stage{{Name: "staging"}, {Name: "prod"}}
	tests := []struct {
		name    string
		ref     string
		wantIdx int
		wantErr bool
	}{
		{"empty defaults to the first stage", "", 0, false},
		{"named stage resolves to its index", "prod", 1, false},
		{"unknown stage errors", "nope", -1, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			idx, err := VersionStageIndex(ss, tc.ref)
			if (err != nil) != tc.wantErr {
				t.Fatalf("VersionStageIndex err = %v, wantErr %v", err, tc.wantErr)
			}
			if idx != tc.wantIdx {
				t.Fatalf("idx = %d, want %d", idx, tc.wantIdx)
			}
		})
	}
}
