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
