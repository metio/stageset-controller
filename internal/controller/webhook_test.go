// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

func stageSetWith(actions *stagesv1.StageActions) *stagesv1.StageSet {
	return &stagesv1.StageSet{
		Spec: stagesv1.StageSetSpec{
			Stages: []stagesv1.Stage{{
				Name:      "s",
				SourceRef: stagesv1.SourceReference{Name: "x"},
				Actions:   actions,
			}},
		},
	}
}

func TestValidateStages(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		action  stagesv1.Action
		wantErr bool
	}{
		{"exactly one (patch)", stagesv1.Action{Name: "a", Patch: &stagesv1.PatchAction{}}, false},
		{"exactly one (wait)", stagesv1.Action{Name: "a", Wait: &stagesv1.WaitAction{}}, false},
		{"none set", stagesv1.Action{Name: "a"}, true},
		{"two set", stagesv1.Action{Name: "a", Patch: &stagesv1.PatchAction{}, Wait: &stagesv1.WaitAction{}}, true},
		{"all set", stagesv1.Action{Name: "a", Patch: &stagesv1.PatchAction{}, HTTP: &stagesv1.HTTPAction{}, Wait: &stagesv1.WaitAction{}, Job: &stagesv1.JobAction{}}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateStages(stageSetWith(&stagesv1.StageActions{Pre: []stagesv1.Action{tc.action}}))
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateStages err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

func TestValidateStages_StageNameUniqueness(t *testing.T) {
	t.Parallel()
	// No spec.version / migrations — the exact path validateMigrations skips, so
	// uniqueness must be enforced by ValidateStages itself.
	dup := &stagesv1.StageSet{
		Spec: stagesv1.StageSetSpec{
			Stages: []stagesv1.Stage{
				{Name: "app", SourceRef: stagesv1.SourceReference{Name: "ea-1"}},
				{Name: "app", SourceRef: stagesv1.SourceReference{Name: "ea-2"}},
			},
		},
	}
	if err := ValidateStages(dup); err == nil {
		t.Fatal("duplicate stage names must be rejected even without migrations")
	}

	ok := &stagesv1.StageSet{
		Spec: stagesv1.StageSetSpec{
			Stages: []stagesv1.Stage{
				{Name: "app", SourceRef: stagesv1.SourceReference{Name: "ea-1"}},
				{Name: "db", SourceRef: stagesv1.SourceReference{Name: "ea-2"}},
			},
		},
	}
	if err := ValidateStages(ok); err != nil {
		t.Fatalf("distinct stage names must validate: %v", err)
	}
}

func TestValidateStages_NoActions(t *testing.T) {
	t.Parallel()
	if err := ValidateStages(stageSetWith(nil)); err != nil {
		t.Fatalf("stage without actions should be valid: %v", err)
	}
}

func TestValidateStages_ChecksEveryPhase(t *testing.T) {
	t.Parallel()
	// A valid pre-action but an empty onFailure action must still fail.
	actions := &stagesv1.StageActions{
		Pre:       []stagesv1.Action{{Name: "ok", Patch: &stagesv1.PatchAction{}}},
		OnFailure: []stagesv1.Action{{Name: "bad"}},
	}
	if err := ValidateStages(stageSetWith(actions)); err == nil {
		t.Fatal("expected onFailure action with no type to be rejected")
	}
}

// Action names are the per-stage idempotency-ledger key, shared across
// pre/post/onFailure, so empty or duplicated names must be rejected at
// validation — otherwise the second action with the same key silently skips.
func TestValidateStages_ActionNameUniqueness(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		actions *stagesv1.StageActions
		wantErr bool
	}{
		{
			name: "distinct names across phases accepted",
			actions: &stagesv1.StageActions{
				Pre:       []stagesv1.Action{{Name: "a", Wait: &stagesv1.WaitAction{}}},
				Post:      []stagesv1.Action{{Name: "b", Wait: &stagesv1.WaitAction{}}},
				OnFailure: []stagesv1.Action{{Name: "c", Wait: &stagesv1.WaitAction{}}},
			},
		},
		{
			name: "empty name rejected",
			actions: &stagesv1.StageActions{
				Pre: []stagesv1.Action{{Name: "", Wait: &stagesv1.WaitAction{}}},
			},
			wantErr: true,
		},
		{
			name: "duplicate within a phase rejected",
			actions: &stagesv1.StageActions{
				Pre: []stagesv1.Action{
					{Name: "dup", Wait: &stagesv1.WaitAction{}},
					{Name: "dup", Wait: &stagesv1.WaitAction{}},
				},
			},
			wantErr: true,
		},
		{
			name: "duplicate across phases rejected",
			actions: &stagesv1.StageActions{
				Pre:  []stagesv1.Action{{Name: "shared", Wait: &stagesv1.WaitAction{}}},
				Post: []stagesv1.Action{{Name: "shared", Wait: &stagesv1.WaitAction{}}},
			},
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateStages(stageSetWith(tc.actions))
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateStages err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

// A delete action satisfies the action oneof as a single verb and is accepted
// (it is implemented, not reserved).
func TestValidateSpec_DeleteVerbAccepted(t *testing.T) {
	t.Parallel()
	ss := stageSetWith(&stagesv1.StageActions{
		Pre: []stagesv1.Action{{Name: "drop", Delete: &stagesv1.DeleteAction{}}},
	})
	if err := ValidateStages(ss); err != nil {
		t.Fatalf("delete should satisfy the action oneof, got %v", err)
	}
	if err := ValidateSpec(ss); err != nil {
		t.Fatalf("delete is implemented; ValidateSpec must accept it, got %v", err)
	}
}

// The reconciler fallback rejects an invalid spec (an action with no type)
// when admission is bypassed — the CRD itself no longer carries the oneof CEL
// rule, so the apiserver accepts the object and the controller must catch it.
func TestReconcile_InvalidActionFallback(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "bad-action"},
		Spec: stagesv1.StageSetSpec{
			Interval: metav1.Duration{Duration: 5 * time.Minute},
			Stages: []stagesv1.Stage{{
				Name:      "s",
				SourceRef: stagesv1.SourceReference{Name: "x"},
				Actions:   &stagesv1.StageActions{Pre: []stagesv1.Action{{Name: "noop"}}},
			}},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet (CRD should accept it without the CEL rule): %v", err)
	}
	reconcileOnce(t, c, ss)

	if r := readyReason(getStageSet(t, c, ns, "bad-action")); r != ReasonInvalidSpec {
		t.Fatalf("Ready reason = %q, want %q", r, ReasonInvalidSpec)
	}
}

// A producer-kind sourceRef (JsonnetSnippet) without apiVersion can never
// resolve — the back-pointer match is on API group. Reject it at admission
// with the actionable message rather than let it fail not-found later.
func TestValidateStages_ProducerSourceRefRequiresAPIVersion(t *testing.T) {
	t.Parallel()
	missing := &stagesv1.StageSet{
		Spec: stagesv1.StageSetSpec{
			Stages: []stagesv1.Stage{
				{Name: "app", SourceRef: stagesv1.SourceReference{Kind: "JsonnetSnippet", Name: "dash"}},
			},
		},
	}
	if err := ValidateStages(missing); err == nil {
		t.Fatal("a producer sourceRef without apiVersion must be rejected")
	} else if !strings.Contains(err.Error(), "apiVersion") {
		t.Errorf("error should name apiVersion, got: %v", err)
	}

	ok := &stagesv1.StageSet{
		Spec: stagesv1.StageSetSpec{
			Stages: []stagesv1.Stage{
				{Name: "app", SourceRef: stagesv1.SourceReference{APIVersion: "jaas.metio.wtf/v1", Kind: "JsonnetSnippet", Name: "dash"}},
			},
		},
	}
	if err := ValidateStages(ok); err != nil {
		t.Fatalf("a producer sourceRef with apiVersion must validate: %v", err)
	}

	// Direct kinds (ExternalArtifact default, GitRepository) need no apiVersion.
	for _, ref := range []stagesv1.SourceReference{
		{Name: "ea"},
		{Kind: "ExternalArtifact", Name: "ea"},
		{Kind: "GitRepository", Name: "repo"},
	} {
		direct := &stagesv1.StageSet{Spec: stagesv1.StageSetSpec{Stages: []stagesv1.Stage{{Name: "app", SourceRef: ref}}}}
		if err := ValidateStages(direct); err != nil {
			t.Errorf("direct source %+v must validate without apiVersion: %v", ref, err)
		}
	}
}
