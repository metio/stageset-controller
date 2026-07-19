// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package imageverify

import (
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

func obj(kind string, spec map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{"kind": kind, "spec": spec}}
}

func podTemplate(containers, initContainers []any) map[string]any {
	ps := map[string]any{}
	if containers != nil {
		ps["containers"] = containers
	}
	if initContainers != nil {
		ps["initContainers"] = initContainers
	}
	return map[string]any{"template": map[string]any{"spec": ps}}
}

func container(name, image string) any { return map[string]any{"name": name, "image": image} }

func TestExtractImages(t *testing.T) {
	objects := []*unstructured.Unstructured{
		obj("Deployment", podTemplate(
			[]any{container("app", "reg.io/app:1.0"), container("proxy", "reg.io/proxy:2")},
			[]any{container("migrate", "reg.io/migrate:1.0")},
		)),
		// A duplicate image (app again) must dedupe.
		obj("StatefulSet", podTemplate([]any{container("app", "reg.io/app:1.0")}, nil)),
		obj("CronJob", map[string]any{"jobTemplate": map[string]any{"spec": podTemplate([]any{container("job", "reg.io/job:3")}, nil)}}),
		obj("Pod", map[string]any{"containers": []any{container("bare", "reg.io/bare:4")}}),
		obj("ConfigMap", map[string]any{"data": map[string]any{"x": "y"}}), // no pod spec
	}
	got := ExtractImages(objects)
	want := []string{"reg.io/app:1.0", "reg.io/bare:4", "reg.io/job:3", "reg.io/migrate:1.0", "reg.io/proxy:2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ExtractImages = %v, want %v", got, want)
	}
}

func TestRefName(t *testing.T) {
	tests := map[string]string{
		"reg.io/app:1.2":                   "reg.io/app",
		"reg.io/app@sha256:abc":            "reg.io/app",
		"reg.io/app:1.2@sha256:abc":        "reg.io/app",
		"localhost:5000/app:1.2":           "localhost:5000/app", // registry port kept
		"localhost:5000/app":               "localhost:5000/app",
		"reg.io/team/app":                  "reg.io/team/app",
		"gcr.io/distroless/static:nonroot": "gcr.io/distroless/static",
	}
	for in, want := range tests {
		if got := refName(in); got != want {
			t.Errorf("refName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGlobMatch(t *testing.T) {
	tests := []struct {
		pattern, s string
		want       bool
	}{
		{"reg.io/**", "reg.io/team/app", true},
		{"reg.io/*", "reg.io/app", true},
		{"reg.io/*", "reg.io/team/app", false}, // single * doesn't cross '/'
		{"reg.io/**", "reg.io/app", true},
		{"gcr.io/distroless/**", "gcr.io/distroless/static", true},
		{"reg.io/app", "reg.io/app", true},
		{"reg.io/app", "reg.io/other", false},
	}
	for _, tc := range tests {
		if got := globMatch(tc.pattern, tc.s); got != tc.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", tc.pattern, tc.s, got, tc.want)
		}
	}
}

func TestMatch(t *testing.T) {
	pol := func(name string, images, skip []string) stagesv1.ImageVerificationPolicy {
		return stagesv1.ImageVerificationPolicy{
			Spec: stagesv1.ImageVerificationPolicySpec{Images: images, Skip: skip},
		}
	}
	policies := []stagesv1.ImageVerificationPolicy{
		pol("infra", []string{"reg.io/**"}, []string{"reg.io/base/**"}),
		pol("apps", []string{"reg.io/apps/**"}, nil),
	}

	t.Run("matches governing policies", func(t *testing.T) {
		matched, skipped := Match(policies, "reg.io/apps/web:1.0")
		if skipped || len(matched) != 2 {
			t.Fatalf("expected both policies to govern, got %d matched, skipped=%v", len(matched), skipped)
		}
	})
	t.Run("skip exempts", func(t *testing.T) {
		matched, skipped := Match(policies, "reg.io/base/distroless:1")
		if !skipped {
			t.Fatal("a Skip-matched image should be reported skipped")
		}
		if len(matched) != 0 {
			t.Fatalf("a skipped image should match no policy for enforcement, got %d", len(matched))
		}
	})
	t.Run("no policy governs", func(t *testing.T) {
		matched, skipped := Match(policies, "other.io/app:1")
		if skipped || len(matched) != 0 {
			t.Fatalf("an unrelated image matches nothing, got %d matched, skipped=%v", len(matched), skipped)
		}
	})
}
