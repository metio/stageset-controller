// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

// scopeSpec wraps a stage (and optional version + onRollback) into a minimal
// valid StageSet so ValidateSpec exercises only the scope rules.
func scopeSpec(version string, stage stagesv1.Stage, onRollback ...stagesv1.Action) *stagesv1.StageSet {
	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "s"},
		Spec:       stagesv1.StageSetSpec{Stages: []stagesv1.Stage{stage}, OnRollback: onRollback},
	}
	if version != "" {
		ss.Spec.Version = &stagesv1.VersionSource{Value: version}
	}
	return ss
}

func httpAction(name string, scope stagesv1.ActionScope) stagesv1.Action {
	return stagesv1.Action{Name: name, Scope: scope, HTTP: &stagesv1.HTTPAction{URL: "http://x/y"}}
}

var sampleAnchor = &stagesv1.CompletionAnchor{APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: "db"}

func anchoredAction(name string, scope stagesv1.ActionScope, anchor *stagesv1.CompletionAnchor) stagesv1.Action {
	a := httpAction(name, scope)
	a.CompletionAnchor = anchor
	return a
}

func TestValidateSpec_ActionScope(t *testing.T) {
	tests := []struct {
		name    string
		ss      *stagesv1.StageSet
		wantErr bool
	}{
		{
			name: "pre Version with spec.version is accepted",
			ss: scopeSpec("1.0.0", stagesv1.Stage{
				Name: "app", SourceRef: stagesv1.SourceReference{Name: "ea"},
				Actions: &stagesv1.StageActions{Pre: []stagesv1.Action{httpAction("upgrade", stagesv1.ScopeVersion)}},
			}),
		},
		{
			name: "post Version with spec.version is accepted",
			ss: scopeSpec("1.0.0", stagesv1.Stage{
				Name: "app", SourceRef: stagesv1.SourceReference{Name: "ea"},
				Actions: &stagesv1.StageActions{Post: []stagesv1.Action{httpAction("upgrade", stagesv1.ScopeVersion)}},
			}),
		},
		{
			name: "Revision needs no version",
			ss: scopeSpec("", stagesv1.Stage{
				Name: "app", SourceRef: stagesv1.SourceReference{Name: "ea"},
				Actions: &stagesv1.StageActions{Pre: []stagesv1.Action{httpAction("check", stagesv1.ScopeRevision)}},
			}),
		},
		{
			name: "Version without spec.version is rejected",
			ss: scopeSpec("", stagesv1.Stage{
				Name: "app", SourceRef: stagesv1.SourceReference{Name: "ea"},
				Actions: &stagesv1.StageActions{Pre: []stagesv1.Action{httpAction("upgrade", stagesv1.ScopeVersion)}},
			}),
			wantErr: true,
		},
		{
			name: "scope on an onFailure action is rejected",
			ss: scopeSpec("1.0.0", stagesv1.Stage{
				Name: "app", SourceRef: stagesv1.SourceReference{Name: "ea"},
				Actions: &stagesv1.StageActions{OnFailure: []stagesv1.Action{httpAction("cleanup", stagesv1.ScopeVersion)}},
			}),
			wantErr: true,
		},
		{
			name: "scope on an onRollback action is rejected",
			ss: scopeSpec("1.0.0",
				stagesv1.Stage{Name: "app", SourceRef: stagesv1.SourceReference{Name: "ea"}},
				httpAction("revert", stagesv1.ScopeRevision)),
			wantErr: true,
		},
		{
			name: "pre Lifetime needs no version",
			ss: scopeSpec("", stagesv1.Stage{
				Name: "app", SourceRef: stagesv1.SourceReference{Name: "ea"},
				Actions: &stagesv1.StageActions{Pre: []stagesv1.Action{httpAction("bootstrap", stagesv1.ScopeLifetime)}},
			}),
		},
		{
			name: "post Lifetime on a versioned StageSet is accepted",
			ss: scopeSpec("1.0.0", stagesv1.Stage{
				Name: "app", SourceRef: stagesv1.SourceReference{Name: "ea"},
				Actions: &stagesv1.StageActions{Post: []stagesv1.Action{httpAction("install-database", stagesv1.ScopeLifetime)}},
			}),
		},
		{
			name: "Lifetime on an onFailure action is rejected",
			ss: scopeSpec("1.0.0", stagesv1.Stage{
				Name: "app", SourceRef: stagesv1.SourceReference{Name: "ea"},
				Actions: &stagesv1.StageActions{OnFailure: []stagesv1.Action{httpAction("cleanup", stagesv1.ScopeLifetime)}},
			}),
			wantErr: true,
		},
		{
			name: "Lifetime on an onRollback action is rejected",
			ss: scopeSpec("1.0.0",
				stagesv1.Stage{Name: "app", SourceRef: stagesv1.SourceReference{Name: "ea"}},
				httpAction("revert", stagesv1.ScopeLifetime)),
			wantErr: true,
		},
		{
			name: "unknown scope value is rejected",
			ss: scopeSpec("1.0.0", stagesv1.Stage{
				Name: "app", SourceRef: stagesv1.SourceReference{Name: "ea"},
				Actions: &stagesv1.StageActions{Pre: []stagesv1.Action{httpAction("x", "Bogus")}},
			}),
			wantErr: true,
		},
		{
			name: "completionAnchor with scope: Lifetime is accepted",
			ss: scopeSpec("1.0.0", stagesv1.Stage{
				Name: "app", SourceRef: stagesv1.SourceReference{Name: "ea"},
				Actions: &stagesv1.StageActions{Post: []stagesv1.Action{anchoredAction("boot", stagesv1.ScopeLifetime, sampleAnchor)}},
			}),
		},
		{
			name: "completionAnchor without scope: Lifetime is rejected",
			ss: scopeSpec("1.0.0", stagesv1.Stage{
				Name: "app", SourceRef: stagesv1.SourceReference{Name: "ea"},
				Actions: &stagesv1.StageActions{Post: []stagesv1.Action{anchoredAction("boot", stagesv1.ScopeRevision, sampleAnchor)}},
			}),
			wantErr: true,
		},
		{
			name: "completionAnchor missing name is rejected",
			ss: scopeSpec("1.0.0", stagesv1.Stage{
				Name: "app", SourceRef: stagesv1.SourceReference{Name: "ea"},
				Actions: &stagesv1.StageActions{Post: []stagesv1.Action{anchoredAction("boot", stagesv1.ScopeLifetime, &stagesv1.CompletionAnchor{APIVersion: "v1", Kind: "PersistentVolumeClaim"})}},
			}),
			wantErr: true,
		},
		{
			name: "completionAnchor on an onRollback action is rejected",
			ss: scopeSpec("1.0.0",
				stagesv1.Stage{Name: "app", SourceRef: stagesv1.SourceReference{Name: "ea"}},
				anchoredAction("revert", "", sampleAnchor)),
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateSpec(tc.ss); (err != nil) != tc.wantErr {
				t.Fatalf("ValidateSpec err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

// A migration action already keyed to its version transition may not also carry
// scope. Validated through the shared migration validator, so it covers inline,
// sourced, and lint-migrations paths.
func TestValidateSpec_ScopeOnMigrationActionRejected(t *testing.T) {
	ss := scopeSpec("2.0.0", stagesv1.Stage{Name: "app", SourceRef: stagesv1.SourceReference{Name: "ea"}})
	ss.Spec.Migrations = []stagesv1.Migration{{
		Name:    "m1",
		To:      "2.0.0",
		Actions: []stagesv1.Action{httpAction("scoped-in-migration", stagesv1.ScopeVersion)},
	}}
	if err := ValidateSpec(ss); err == nil {
		t.Fatal("scope on a migration action must be rejected")
	}
}

// completionAnchor is valid only on a scope: Lifetime pre/post action; a
// migration action carries no lifetime ledger, so it is rejected there too.
func TestValidateSpec_CompletionAnchorOnMigrationRejected(t *testing.T) {
	ss := scopeSpec("2.0.0", stagesv1.Stage{Name: "app", SourceRef: stagesv1.SourceReference{Name: "ea"}})
	ss.Spec.Migrations = []stagesv1.Migration{{
		Name:    "m1",
		To:      "2.0.0",
		Actions: []stagesv1.Action{anchoredAction("anchored-in-migration", "", sampleAnchor)},
	}}
	if err := ValidateSpec(ss); err == nil {
		t.Fatal("completionAnchor on a migration action must be rejected")
	}
}
