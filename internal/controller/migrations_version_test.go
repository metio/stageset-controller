// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

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
			got := findVersionObject(objects, &tt.ref)
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
		o.SetLabels(map[string]string{versionLabel: "2.1.0"})
	})

	t.Run("default reads the version label", func(t *testing.T) {
		got, err := extractVersionField(labeled, "")
		if err != nil || got != "2.1.0" {
			t.Fatalf("got (%q, %v), want 2.1.0", got, err)
		}
	})
	t.Run("missing label is InvalidVersion", func(t *testing.T) {
		_, err := extractVersionField(obj("v1", "ConfigMap", "c", nil), "")
		if err == nil || !strings.Contains(err.Error(), versionLabel) {
			t.Fatalf("expected an InvalidVersion error naming the label, got %v", err)
		}
	})
	t.Run("jsonpath fieldPath reads an arbitrary field", func(t *testing.T) {
		got, err := extractVersionField(labeled, "{.metadata.labels.app\\.kubernetes\\.io/version}")
		if err != nil || got != "2.1.0" {
			t.Fatalf("got (%q, %v), want 2.1.0", got, err)
		}
	})
	t.Run("invalid jsonpath errors", func(t *testing.T) {
		if _, err := extractVersionField(labeled, "{.unterminated"); err == nil {
			t.Fatal("expected a parse error for malformed JSONPath")
		}
	})
}

func TestValidateVersion(t *testing.T) {
	tests := []struct {
		name    string
		version *stagesv1.VersionSource
		wantErr bool
	}{
		{name: "nil is fine", version: nil},
		{name: "value only", version: &stagesv1.VersionSource{Value: "1.0.0"}},
		{name: "fromObject only", version: &stagesv1.VersionSource{FromObject: &stagesv1.ObjectVersionRef{Stage: "a", Kind: "Deployment", Name: "web"}}},
		{name: "fromArtifact only", version: &stagesv1.VersionSource{FromArtifact: &stagesv1.ArtifactVersionRef{Stage: "a", Path: "VERSION"}}},
		{name: "none set", version: &stagesv1.VersionSource{}, wantErr: true},
		{
			name:    "two set",
			version: &stagesv1.VersionSource{Value: "1.0.0", FromObject: &stagesv1.ObjectVersionRef{Stage: "a", Kind: "Deployment", Name: "web"}},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ss := &stagesv1.StageSet{}
			ss.Spec.Version = tt.version
			err := validateVersion(ss)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateVersion err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
