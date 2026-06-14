// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package inventory

import (
	"regexp"
	"testing"
)

// idPattern: unpadded URL-safe base64 of a SHA-256 digest is 43 characters.
var idPattern = regexp.MustCompile(`^applyset-[A-Za-z0-9_-]{43}-v1$`)

func TestApplySetIDFormat(t *testing.T) {
	t.Parallel()
	id := ApplySetID("platform-operators-00", "flux-system", "StageInventory", "stages.metio.wtf")
	if !idPattern.MatchString(id) {
		t.Fatalf("ApplySetID %q does not match the V1 spec format", id)
	}
}

func TestApplySetIDIsDeterministicAndDiscriminating(t *testing.T) {
	t.Parallel()
	base := ApplySetID("parent", "ns", "StageInventory", "stages.metio.wtf")
	if base != ApplySetID("parent", "ns", "StageInventory", "stages.metio.wtf") {
		t.Fatal("ApplySetID is not deterministic")
	}
	variants := []string{
		ApplySetID("parent2", "ns", "StageInventory", "stages.metio.wtf"),
		ApplySetID("parent", "ns2", "StageInventory", "stages.metio.wtf"),
		ApplySetID("parent", "ns", "ConfigMap", ""),
	}
	for _, variant := range variants {
		if variant == base {
			t.Fatalf("ApplySetID collision: %q", variant)
		}
	}
}

func TestContainsGroupKinds(t *testing.T) {
	t.Parallel()
	entries := []ObjectRef{
		{Group: "apps", Kind: "Deployment", Namespace: "a", Name: "x"},
		{Group: "apps", Kind: "Deployment", Namespace: "b", Name: "y"}, // duplicate group-kind
		{Group: "", Kind: "Service", Namespace: "a", Name: "x"},
		{Group: "cert-manager.io", Kind: "ClusterIssuer", Name: "le"},
	}
	want := "ClusterIssuer.cert-manager.io,Deployment.apps,Service"
	if got := ContainsGroupKinds(entries); got != want {
		t.Fatalf("ContainsGroupKinds = %q, want %q", got, want)
	}
	if got := ContainsGroupKinds(nil); got != "" {
		t.Fatalf("empty entries should render empty hint, got %q", got)
	}
}

func TestAdditionalNamespaces(t *testing.T) {
	t.Parallel()
	entries := []ObjectRef{
		{Kind: "ConfigMap", Namespace: "flux-system", Name: "in-parent-ns"},
		{Kind: "ConfigMap", Namespace: "webapp", Name: "x"},
		{Kind: "ConfigMap", Namespace: "billing", Name: "y"},
		{Kind: "ConfigMap", Namespace: "billing", Name: "z"}, // duplicate namespace
		{Kind: "ClusterIssuer", Group: "cert-manager.io", Name: "cluster-scoped"},
	}
	want := "billing,webapp"
	if got := AdditionalNamespaces(entries, "flux-system"); got != want {
		t.Fatalf("AdditionalNamespaces = %q, want %q", got, want)
	}
}
