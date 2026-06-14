// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package inventory

import "testing"

func TestObjectRefID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		ref  ObjectRef
		want string
	}{
		{
			name: "namespaced grouped",
			ref:  ObjectRef{Group: "apps", Kind: "Deployment", Namespace: "billing", Name: "billing-api"},
			want: "billing_billing-api_apps_Deployment",
		},
		{
			name: "namespaced core group",
			ref:  ObjectRef{Group: "", Kind: "ConfigMap", Namespace: "webapp", Name: "webapp-config"},
			want: "webapp_webapp-config__ConfigMap",
		},
		{
			name: "cluster scoped",
			ref:  ObjectRef{Group: "cert-manager.io", Kind: "ClusterIssuer", Namespace: "", Name: "letsencrypt"},
			want: "_letsencrypt_cert-manager.io_ClusterIssuer",
		},
		{
			name: "rbac name with colon",
			ref:  ObjectRef{Group: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "system:metrics"},
			want: "_system:metrics_rbac.authorization.k8s.io_ClusterRole",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.ref.ID(); got != tc.want {
				t.Fatalf("ID() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseIDRoundTrip(t *testing.T) {
	t.Parallel()
	refs := []ObjectRef{
		{Group: "apps", Kind: "Deployment", Namespace: "billing", Name: "billing-api", Version: "v1"},
		{Group: "", Kind: "Service", Namespace: "webapp", Name: "webapp", Version: "v1"},
		{Group: "stages.metio.wtf", Kind: "StageSet", Namespace: "flux-system", Name: "platform", Version: "v1"},
		{Group: "cert-manager.io", Kind: "ClusterIssuer", Name: "letsencrypt", Version: "v1"},
	}
	for _, want := range refs {
		got, err := ParseID(want.ID(), want.Version)
		if err != nil {
			t.Fatalf("ParseID(%q): unexpected error: %v", want.ID(), err)
		}
		if got != want {
			t.Fatalf("round trip: got %+v, want %+v", got, want)
		}
	}
}

func TestParseIDRejectsMalformedInput(t *testing.T) {
	t.Parallel()
	for _, id := range []string{
		"",
		"justone",
		"too_few_parts",
		"way_too_many_parts_here_yes",
		"ns__apps_Deployment", // empty name
		"ns_name_apps_",       // empty kind
	} {
		if _, err := ParseID(id, "v1"); err == nil {
			t.Errorf("ParseID(%q): expected error, got nil", id)
		}
	}
}

func TestClusterScoped(t *testing.T) {
	t.Parallel()
	if !(ObjectRef{Kind: "Namespace", Name: "x"}).ClusterScoped() {
		t.Error("expected cluster-scoped")
	}
	if (ObjectRef{Kind: "Pod", Namespace: "x", Name: "y"}).ClusterScoped() {
		t.Error("expected namespaced")
	}
}
