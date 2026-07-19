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

func TestValidateDependsOn(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		dep     stagesv1.Dependency
		wantErr bool
	}{
		{"readiness-only", stagesv1.Dependency{Name: "db"}, false},
		{"valid minVersion", stagesv1.Dependency{Name: "db", MinVersion: "2.0.0"}, false},
		{"empty name", stagesv1.Dependency{MinVersion: "2.0.0"}, true},
		{"invalid minVersion", stagesv1.Dependency{Name: "db", MinVersion: "two"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ss := &stagesv1.StageSet{Spec: stagesv1.StageSetSpec{DependsOn: []stagesv1.Dependency{tc.dep}}}
			if err := validateDependsOn(ss); (err != nil) != tc.wantErr {
				t.Fatalf("validateDependsOn err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
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

// spec.onRollback is a StageSet-level action list validated for the oneof verb
// and name uniqueness within itself, independent of the per-stage phases.
func TestValidateStages_OnRollback(t *testing.T) {
	t.Parallel()
	withOnRollback := func(acts ...stagesv1.Action) *stagesv1.StageSet {
		ss := stageSetWith(nil)
		ss.Spec.OnRollback = acts
		return ss
	}
	tests := []struct {
		name    string
		ss      *stagesv1.StageSet
		wantErr bool
	}{
		{"valid single action", withOnRollback(stagesv1.Action{Name: "disable-maintenance", Job: &stagesv1.JobAction{}}), false},
		{"no verb set rejected", withOnRollback(stagesv1.Action{Name: "bad"}), true},
		{"two verbs rejected", withOnRollback(stagesv1.Action{Name: "bad", Job: &stagesv1.JobAction{}, HTTP: &stagesv1.HTTPAction{}}), true},
		{"empty name rejected", withOnRollback(stagesv1.Action{Name: "", Wait: &stagesv1.WaitAction{}}), true},
		{"duplicate name rejected", withOnRollback(
			stagesv1.Action{Name: "dup", Wait: &stagesv1.WaitAction{}},
			stagesv1.Action{Name: "dup", Wait: &stagesv1.WaitAction{}},
		), true},
		{"shares a name with a stage action (own namespace)", func() *stagesv1.StageSet {
			ss := stageSetWith(&stagesv1.StageActions{Pre: []stagesv1.Action{{Name: "same", Wait: &stagesv1.WaitAction{}}}})
			ss.Spec.OnRollback = []stagesv1.Action{{Name: "same", Wait: &stagesv1.WaitAction{}}}
			return ss
		}(), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := ValidateStages(tc.ss); (err != nil) != tc.wantErr {
				t.Fatalf("ValidateStages err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
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

// An update that changes only spec.suspend (or only metadata) on a StageSet that
// is ALREADY invalid — a violation introduced out-of-band, e.g. an operator
// upgrade that tightened a check under an object admitted by an older binary —
// must be admitted. Suspend and the reconcile annotation are the documented
// remediations for a wedged StageSet, and the MCP suspend/resume/reconcile tools
// patch the main resource through this webhook; re-validating an unchanged
// violation would deny the very fix.
func TestValidateUpdate_SuspendTogglePassesDespiteStaleViolation(t *testing.T) {
	t.Parallel()
	v := &StageSetValidator{}
	// Duplicate stage names: invalid under ValidateStages, but present on both
	// old and new (the update does not touch it).
	invalid := stagesv1.StageSetSpec{
		Stages: []stagesv1.Stage{
			{Name: "app", SourceRef: stagesv1.SourceReference{Name: "ea-1"}},
			{Name: "app", SourceRef: stagesv1.SourceReference{Name: "ea-2"}},
		},
	}
	old := &stagesv1.StageSet{Spec: invalid}
	updated := old.DeepCopy()
	updated.Spec.Suspend = true // the only change

	if _, err := v.ValidateUpdate(context.Background(), old, updated); err != nil {
		t.Fatalf("suspend toggle on an already-invalid StageSet must be admitted: %v", err)
	}

	// A metadata-only update (spec byte-identical) is likewise admitted.
	annotated := old.DeepCopy()
	annotated.Annotations = map[string]string{"reconcile.fluxcd.io/requestedAt": "now"}
	if _, err := v.ValidateUpdate(context.Background(), old, annotated); err != nil {
		t.Fatalf("metadata-only update on an already-invalid StageSet must be admitted: %v", err)
	}
}

// The escape hatch is narrow: an update that CHANGES a validated spec field is
// still fully re-validated, so a suspend toggle cannot smuggle a new violation
// past admission.
func TestValidateUpdate_ChangingValidatedInputStillValidated(t *testing.T) {
	t.Parallel()
	v := &StageSetValidator{}
	old := &stagesv1.StageSet{Spec: stagesv1.StageSetSpec{
		Stages: []stagesv1.Stage{{Name: "app", SourceRef: stagesv1.SourceReference{Name: "ea-1"}}},
	}}
	// Introduce a duplicate stage name in the same update that flips suspend.
	updated := old.DeepCopy()
	updated.Spec.Suspend = true
	updated.Spec.Stages = append(updated.Spec.Stages, stagesv1.Stage{Name: "app", SourceRef: stagesv1.SourceReference{Name: "ea-2"}})

	if _, err := v.ValidateUpdate(context.Background(), old, updated); err == nil {
		t.Fatal("an update that introduces a duplicate stage name must still be rejected")
	}
}

// A finalizer-removal update on a terminating StageSet must be admitted even if
// its spec is invalid, or the object wedges in Terminating.
func TestValidateUpdate_DeletingObjectIsAdmitted(t *testing.T) {
	t.Parallel()
	v := &StageSetValidator{}
	now := metav1.Now()
	invalid := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{DeletionTimestamp: &now, Finalizers: []string{"x"}},
		Spec: stagesv1.StageSetSpec{Stages: []stagesv1.Stage{
			{Name: "app", SourceRef: stagesv1.SourceReference{Name: "ea-1"}},
			{Name: "app", SourceRef: stagesv1.SourceReference{Name: "ea-2"}},
		}},
	}
	if _, err := v.ValidateUpdate(context.Background(), invalid, invalid); err != nil {
		t.Fatalf("update of a deleting StageSet must be admitted: %v", err)
	}
}
