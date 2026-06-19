// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/fluxcd/pkg/apis/meta"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

// validationStageSet builds a minimal, otherwise-valid StageSet whose single
// stage callers mutate to plant the field under test. Keeping the rest valid
// isolates the apiserver's rejection to the field being probed. The name is a
// fixed valid RFC1123 string — the probed enum value goes in the spec, never in
// the object name, so a rejection can only come from the field under test.
func validationStageSet(ns, name string) *stagesv1.StageSet {
	return &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: stagesv1.StageSetSpec{
			Interval: metav1.Duration{Duration: time.Minute},
			Stages: []stagesv1.Stage{{
				Name:      "stage-a",
				SourceRef: stagesv1.SourceReference{Name: "bundle"},
			}},
		},
	}
}

// createOutcome creates ss and classifies the result: nil error (accepted) or
// an Invalid validation error (rejected by the schema). Any other error fails
// the test — it signals an apiserver/setup problem rather than the schema
// verdict the test is pinning.
func createOutcome(t *testing.T, ss *stagesv1.StageSet) (accepted bool) {
	t.Helper()
	c := testClient(t)
	err := c.Create(context.Background(), ss)
	if err == nil {
		return true
	}
	if !apierrors.IsInvalid(err) {
		t.Fatalf("create %q: want either success or an Invalid validation error, got %T: %v", ss.Name, err, err)
	}
	return false
}

// TestCRDEnum_PatchActionType pins the +kubebuilder:validation:Enum=merge;json6902
// marker on PatchAction.Type: the apiserver accepts the listed values and
// rejects anything else.
func TestCRDEnum_PatchActionType(t *testing.T) {
	ns := newNamespace(t, testClient(t))
	n := 0
	patch := func(typ string) *stagesv1.StageSet {
		n++
		ss := validationStageSet(ns, fmt.Sprintf("patch-%d", n))
		ss.Spec.Stages[0].Actions = &stagesv1.StageActions{Pre: []stagesv1.Action{{
			Name: "p",
			Patch: &stagesv1.PatchAction{
				Target: meta.NamespacedObjectKindReference{APIVersion: "v1", Kind: "ConfigMap", Name: "cm"},
				Type:   typ,
				Patch:  "{}",
			},
		}}}
		return ss
	}
	for _, valid := range []string{"merge", "json6902"} {
		if !createOutcome(t, patch(valid)) {
			t.Errorf("PatchAction.Type %q must be accepted", valid)
		}
	}
	for _, bad := range []string{"strategic", "MERGE", "json"} {
		if createOutcome(t, patch(bad)) {
			t.Errorf("PatchAction.Type %q must be rejected by the enum", bad)
		}
	}
}

// TestCRDEnum_SubstituteReferenceKind pins the Enum=ConfigMap;Secret marker on
// SubstituteReference.Kind.
func TestCRDEnum_SubstituteReferenceKind(t *testing.T) {
	ns := newNamespace(t, testClient(t))
	n := 0
	withKind := func(kind string) *stagesv1.StageSet {
		n++
		ss := validationStageSet(ns, fmt.Sprintf("subst-%d", n))
		ss.Spec.Stages[0].PostBuild = &stagesv1.PostBuild{
			SubstituteFrom: []stagesv1.SubstituteReference{{Kind: kind, Name: "vars"}},
		}
		return ss
	}
	for _, valid := range []string{"ConfigMap", "Secret"} {
		if !createOutcome(t, withKind(valid)) {
			t.Errorf("SubstituteReference.Kind %q must be accepted", valid)
		}
	}
	for _, bad := range []string{"configmap", "secret", "Pod", "x"} {
		if createOutcome(t, withKind(bad)) {
			t.Errorf("SubstituteReference.Kind %q must be rejected by the enum", bad)
		}
	}
}

// TestCRDEnum_WindowScope pins the Enum=Updates;All marker on spec.windowScope.
func TestCRDEnum_WindowScope(t *testing.T) {
	ns := newNamespace(t, testClient(t))
	n := 0
	withScope := func(scope string) *stagesv1.StageSet {
		n++
		ss := validationStageSet(ns, fmt.Sprintf("scope-%d", n))
		ss.Spec.WindowScope = scope
		return ss
	}
	for _, valid := range []string{"Updates", "All"} {
		if !createOutcome(t, withScope(valid)) {
			t.Errorf("windowScope %q must be accepted", valid)
		}
	}
	for _, bad := range []string{"updates", "all", "None", "x"} {
		if createOutcome(t, withScope(bad)) {
			t.Errorf("windowScope %q must be rejected by the enum", bad)
		}
	}
}

// TestCRDEnum_UpdateWindowType pins the Enum=Allow;Deny marker on
// UpdateWindow.Type at the apiserver (the runtime behaviour is covered in
// windows_test.go; this pins the schema rejection of an unknown type).
func TestCRDEnum_UpdateWindowType(t *testing.T) {
	ns := newNamespace(t, testClient(t))
	n := 0
	withType := func(typ string) *stagesv1.StageSet {
		n++
		ss := validationStageSet(ns, fmt.Sprintf("win-%d", n))
		ss.Spec.UpdateWindows = []stagesv1.UpdateWindow{{
			Type: typ,
			From: &metav1.Time{Time: time.Unix(0, 0)},
			To:   &metav1.Time{Time: time.Unix(1<<31, 0)},
		}}
		return ss
	}
	for _, valid := range []string{"Allow", "Deny"} {
		if !createOutcome(t, withType(valid)) {
			t.Errorf("UpdateWindow.Type %q must be accepted", valid)
		}
	}
	for _, bad := range []string{"allow", "deny", "Block", "x"} {
		if createOutcome(t, withType(bad)) {
			t.Errorf("UpdateWindow.Type %q must be rejected by the enum", bad)
		}
	}
}

// TestCRDEnum_ConflictPolicy pins the Enum=Fail;Recreate;KeepExisting marker on
// ConflictPolicy.Default and ConflictRule.Action.
func TestCRDEnum_ConflictPolicy(t *testing.T) {
	ns := newNamespace(t, testClient(t))
	n := 0

	withDefault := func(action string) *stagesv1.StageSet {
		n++
		ss := validationStageSet(ns, fmt.Sprintf("cpdef-%d", n))
		ss.Spec.Stages[0].ConflictPolicy = &stagesv1.ConflictPolicy{Default: action}
		return ss
	}
	withRuleAction := func(action string) *stagesv1.StageSet {
		n++
		ss := validationStageSet(ns, fmt.Sprintf("cprule-%d", n))
		ss.Spec.Stages[0].ConflictPolicy = &stagesv1.ConflictPolicy{
			Rules: []stagesv1.ConflictRule{{
				Target: stagesv1.ConflictTarget{Kind: "ConfigMap"},
				Action: action,
			}},
		}
		return ss
	}

	for _, valid := range []string{"Fail", "Recreate", "KeepExisting"} {
		if !createOutcome(t, withDefault(valid)) {
			t.Errorf("ConflictPolicy.Default %q must be accepted", valid)
		}
		if !createOutcome(t, withRuleAction(valid)) {
			t.Errorf("ConflictRule.Action %q must be accepted", valid)
		}
	}
	for _, bad := range []string{"fail", "Delete", "keepexisting", "x"} {
		if createOutcome(t, withDefault(bad)) {
			t.Errorf("ConflictPolicy.Default %q must be rejected by the enum", bad)
		}
		if createOutcome(t, withRuleAction(bad)) {
			t.Errorf("ConflictRule.Action %q must be rejected by the enum", bad)
		}
	}
}

// TestCRDEnum_DecryptionProvider pins the Enum=sops marker on
// Decryption.Provider.
func TestCRDEnum_DecryptionProvider(t *testing.T) {
	ns := newNamespace(t, testClient(t))
	n := 0
	withProvider := func(p string) *stagesv1.StageSet {
		n++
		ss := validationStageSet(ns, fmt.Sprintf("dec-%d", n))
		ss.Spec.Decryption = &stagesv1.Decryption{Provider: p}
		return ss
	}
	if !createOutcome(t, withProvider("sops")) {
		t.Error(`Decryption.Provider "sops" must be accepted`)
	}
	for _, bad := range []string{"SOPS", "vault", "age", "x"} {
		if createOutcome(t, withProvider(bad)) {
			t.Errorf("Decryption.Provider %q must be rejected by the enum", bad)
		}
	}
}

// TestCRDMinItems_StagesRejectsEmpty pins the +kubebuilder:validation:MinItems=1
// marker on spec.stages: a StageSet with no stages is rejected at admission.
func TestCRDMinItems_StagesRejectsEmpty(t *testing.T) {
	ns := newNamespace(t, testClient(t))
	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "no-stages"},
		Spec: stagesv1.StageSetSpec{
			Interval: metav1.Duration{Duration: time.Minute},
			Stages:   []stagesv1.Stage{},
		},
	}
	if createOutcome(t, ss) {
		t.Fatal("a StageSet with zero stages must be rejected by MinItems=1")
	}
	// And one stage is accepted (the positive half of the constraint).
	if !createOutcome(t, validationStageSet(ns, "one-stage")) {
		t.Fatal("a StageSet with one stage must be accepted")
	}
}
