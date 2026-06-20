// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"testing"

	"github.com/fluxcd/pkg/apis/meta"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

// TestValidateDecryption covers the runtime decryption validator: the only
// supported provider is "sops", and a referenced secretRef must carry a name.
// secretRef itself is optional (cloud-KMS-only decryption needs no in-cluster
// Secret).
func TestValidateDecryption(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		decryption *stagesv1.Decryption
		wantErr    bool
	}{
		{name: "nil is fine", decryption: nil},
		{name: "sops without secretRef (cloud KMS)", decryption: &stagesv1.Decryption{Provider: "sops"}},
		{
			name:       "sops with named secretRef",
			decryption: &stagesv1.Decryption{Provider: "sops", SecretRef: &meta.LocalObjectReference{Name: "sops-keys"}},
		},
		{
			name:       "wrong provider rejected",
			decryption: &stagesv1.Decryption{Provider: "vault"},
			wantErr:    true,
		},
		{
			name:       "empty provider rejected",
			decryption: &stagesv1.Decryption{Provider: ""},
			wantErr:    true,
		},
		{
			name:       "secretRef without name rejected",
			decryption: &stagesv1.Decryption{Provider: "sops", SecretRef: &meta.LocalObjectReference{Name: ""}},
			wantErr:    true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ss := &stagesv1.StageSet{}
			ss.Spec.Decryption = tc.decryption
			err := validateDecryption(ss)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateDecryption err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

// TestValidateKubeConfig covers the kubeConfig validator's shape checks:
// exactly one of secretRef / configMapRef, each carrying a name. configMapRef
// (cloud-provider auth) is now accepted at admission — its ConfigMap contents
// can't be read here, so provider/key validation happens at reconcile time.
func TestValidateKubeConfig(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		kubeConfig *meta.KubeConfigReference
		wantErr    bool
	}{
		{name: "nil is fine", kubeConfig: nil},
		{
			name:       "secretRef accepted",
			kubeConfig: &meta.KubeConfigReference{SecretRef: &meta.SecretKeyReference{Name: "remote-kubeconfig"}},
		},
		{
			name:       "configMapRef accepted",
			kubeConfig: &meta.KubeConfigReference{ConfigMapRef: &meta.LocalObjectReference{Name: "cloud-auth"}},
		},
		{
			name:       "configMapRef without name rejected",
			kubeConfig: &meta.KubeConfigReference{ConfigMapRef: &meta.LocalObjectReference{}},
			wantErr:    true,
		},
		{
			name:       "secretRef without name rejected",
			kubeConfig: &meta.KubeConfigReference{SecretRef: &meta.SecretKeyReference{}},
			wantErr:    true,
		},
		{
			name:       "both refs rejected",
			kubeConfig: &meta.KubeConfigReference{SecretRef: &meta.SecretKeyReference{Name: "s"}, ConfigMapRef: &meta.LocalObjectReference{Name: "c"}},
			wantErr:    true,
		},
		{
			name:       "neither ref rejected",
			kubeConfig: &meta.KubeConfigReference{},
			wantErr:    true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ss := &stagesv1.StageSet{}
			ss.Spec.KubeConfig = tc.kubeConfig
			err := validateKubeConfig(ss)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateKubeConfig err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

// TestValidateMigrations_ActionOneof covers the action-oneof branch of
// validateMigrations: a migration whose action sets no verb (or more than one)
// is rejected, while a single-verb migration action passes. The
// version/known-stage branches are covered separately.
func TestValidateMigrations_ActionOneof(t *testing.T) {
	t.Parallel()
	base := func(actions []stagesv1.Action) *stagesv1.StageSet {
		return &stagesv1.StageSet{Spec: stagesv1.StageSetSpec{
			Version: &stagesv1.VersionSource{Value: "1.0.0"},
			Stages:  []stagesv1.Stage{{Name: "s", SourceRef: stagesv1.SourceReference{Name: "x"}}},
			Migrations: []stagesv1.Migration{{
				Name: "m", To: "2.0.0", Stage: "s", Actions: actions,
			}},
		}}
	}
	tests := []struct {
		name    string
		actions []stagesv1.Action
		wantErr bool
	}{
		{name: "single verb accepted", actions: []stagesv1.Action{{Name: "drop", Delete: &stagesv1.DeleteAction{}}}},
		{name: "no actions accepted (Actions is optional)", actions: nil},
		{name: "action with no verb rejected", actions: []stagesv1.Action{{Name: "noop"}}, wantErr: true},
		{
			name:    "action with two verbs rejected",
			actions: []stagesv1.Action{{Name: "two", Delete: &stagesv1.DeleteAction{}, Wait: &stagesv1.WaitAction{}}},
			wantErr: true,
		},
		{
			name:    "empty action name rejected",
			actions: []stagesv1.Action{{Name: "", Delete: &stagesv1.DeleteAction{}}},
			wantErr: true,
		},
		{
			name: "duplicate action names rejected",
			actions: []stagesv1.Action{
				{Name: "dup", Delete: &stagesv1.DeleteAction{}},
				{Name: "dup", Wait: &stagesv1.WaitAction{}},
			},
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateMigrations(base(tc.actions))
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateMigrations err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

// TestValidateMigrations_SourceAndAnchors covers the sourced-ladder admission
// rules: inline vs source mutual exclusivity, the version requirement, that a
// sourced ladder's content is deferred to fetch time, late-binding anchors
// (empty = before first stage; match by Name or MigrationAnchor), and
// anchor-key uniqueness.
func TestValidateMigrations_SourceAndAnchors(t *testing.T) {
	t.Parallel()
	ver := &stagesv1.VersionSource{}
	mig := func(stage string) []stagesv1.Migration {
		return []stagesv1.Migration{{Name: "m", To: "2.0.0", Stage: stage}}
	}
	src := &stagesv1.MigrationsSource{SourceRef: stagesv1.SourceReference{Name: "ladder"}}
	spec := func(s stagesv1.StageSetSpec) *stagesv1.StageSet { return &stagesv1.StageSet{Spec: s} }

	tests := []struct {
		name    string
		ss      *stagesv1.StageSet
		wantErr bool
	}{
		{name: "no migrations is fine", ss: spec(stagesv1.StageSetSpec{})},
		{
			name: "inline + source are mutually exclusive",
			ss: spec(stagesv1.StageSetSpec{
				Version: ver, Stages: []stagesv1.Stage{{Name: "deploy"}},
				Migrations: mig("deploy"), MigrationsSourceRef: src,
			}),
			wantErr: true,
		},
		{
			name:    "source requires version",
			ss:      spec(stagesv1.StageSetSpec{Stages: []stagesv1.Stage{{Name: "deploy"}}, MigrationsSourceRef: src}),
			wantErr: true,
		},
		{
			name: "source with version is fine (content checked at fetch time)",
			ss:   spec(stagesv1.StageSetSpec{Version: ver, Stages: []stagesv1.Stage{{Name: "deploy"}}, MigrationsSourceRef: src}),
		},
		{
			name:    "inline requires version",
			ss:      spec(stagesv1.StageSetSpec{Stages: []stagesv1.Stage{{Name: "deploy"}}, Migrations: mig("deploy")}),
			wantErr: true,
		},
		{
			name: "empty stage anchors before first stage",
			ss:   spec(stagesv1.StageSetSpec{Version: ver, Stages: []stagesv1.Stage{{Name: "deploy"}}, Migrations: mig("")}),
		},
		{
			name: "anchor by migrationAnchor alias",
			ss: spec(stagesv1.StageSetSpec{
				Version:    ver,
				Stages:     []stagesv1.Stage{{Name: "deploy", MigrationAnchor: "db-pre"}},
				Migrations: mig("db-pre"),
			}),
		},
		{
			name:    "unknown anchor rejected",
			ss:      spec(stagesv1.StageSetSpec{Version: ver, Stages: []stagesv1.Stage{{Name: "deploy"}}, Migrations: mig("nope")}),
			wantErr: true,
		},
		{
			name: "anchor alias colliding with another stage name rejected",
			ss: spec(stagesv1.StageSetSpec{
				Version: ver,
				Stages: []stagesv1.Stage{
					{Name: "deploy"},
					{Name: "verify", MigrationAnchor: "deploy"},
				},
				Migrations: mig("deploy"),
			}),
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := validateMigrations(tc.ss); (err != nil) != tc.wantErr {
				t.Fatalf("validateMigrations err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}
