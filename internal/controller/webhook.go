// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"fmt"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/window"
)

// +kubebuilder:webhook:path=/validate-stages-metio-wtf-v1-stageset,mutating=false,failurePolicy=fail,sideEffects=None,groups=stages.metio.wtf,resources=stagesets,verbs=create;update,versions=v1,name=vstageset.stages.metio.wtf,admissionReviewVersions=v1

// StageSetValidator is the validating admission webhook for StageSet. It
// enforces invariants the CRD schema cannot express cheaply — currently that
// every action sets exactly one of patch/http/wait/job. Keeping this out of a
// CRD CEL rule is what lets spec.stages and the action lists stay unbounded:
// CEL validation cost is multiplied by the size of every enclosing array, so
// an unbounded list makes the apiserver reject the CRD.
type StageSetValidator struct{}

var _ admission.Validator[*stagesv1.StageSet] = &StageSetValidator{}

// ValidateCreate validates a new StageSet.
func (v *StageSetValidator) ValidateCreate(_ context.Context, ss *stagesv1.StageSet) (admission.Warnings, error) {
	return nil, ValidateSpec(ss)
}

// ValidateUpdate validates an updated StageSet.
func (v *StageSetValidator) ValidateUpdate(_ context.Context, _, newObj *stagesv1.StageSet) (admission.Warnings, error) {
	return nil, ValidateSpec(newObj)
}

// ValidateDelete is a no-op; deletion carries no spec to validate.
func (v *StageSetValidator) ValidateDelete(_ context.Context, _ *stagesv1.StageSet) (admission.Warnings, error) {
	return nil, nil
}

// SetupWebhookWithManager registers the validating webhook on the manager's
// webhook server.
func (v *StageSetValidator) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &stagesv1.StageSet{}).
		WithValidator(v).
		Complete()
}

// ValidateSpec is the single source of truth for spec validation, shared by
// the admission webhook and the reconciler fallback (so a StageSet that slips
// past a bypassed/disabled webhook still fails loudly). It enforces the action
// oneof invariant and rejects reserved post-v1 fields.
func ValidateSpec(ss *stagesv1.StageSet) error {
	if err := ValidateStages(ss); err != nil {
		return err
	}
	if err := validateMigrations(ss); err != nil {
		return err
	}
	if err := validateVersion(ss); err != nil {
		return err
	}
	if err := validateDecryption(ss); err != nil {
		return err
	}
	return window.Validate(ss.Spec.UpdateWindows)
}

// validateDecryption enforces that spec.decryption, when set, names the only
// supported provider, and that a referenced secretRef carries a name. secretRef
// itself is optional: a cloud-KMS-only setup decrypts through the controller's
// ambient credentials with no in-cluster key Secret.
func validateDecryption(ss *stagesv1.StageSet) error {
	d := ss.Spec.Decryption
	if d == nil {
		return nil
	}
	if d.Provider != "sops" {
		return fmt.Errorf("spec.decryption.provider must be \"sops\", got %q", d.Provider)
	}
	if d.SecretRef != nil && d.SecretRef.Name == "" {
		return fmt.Errorf("spec.decryption.secretRef.name must be set when secretRef is given")
	}
	return nil
}

// validateVersion enforces that spec.version, when set, names exactly one
// source: value, fromObject, or fromArtifact. The reconciler's resolver applies
// a precedence order as a fallback, but admission rejects an ambiguous or empty
// version source up front.
func validateVersion(ss *stagesv1.StageSet) error {
	v := ss.Spec.Version
	if v == nil {
		return nil
	}
	n := 0
	if v.Value != "" {
		n++
	}
	if v.FromObject != nil {
		n++
	}
	if v.FromArtifact != nil {
		n++
	}
	if n != 1 {
		return fmt.Errorf("spec.version must set exactly one of value, fromObject, or fromArtifact, found %d", n)
	}
	return nil
}

// validateMigrations enforces that migrations require a version, anchor to a
// real stage, and that each migration action sets exactly one verb.
func validateMigrations(ss *stagesv1.StageSet) error {
	if len(ss.Spec.Migrations) == 0 {
		return nil
	}
	if ss.Spec.Version == nil {
		return fmt.Errorf("spec.migrations requires spec.version")
	}
	stages := make(map[string]bool, len(ss.Spec.Stages))
	for i := range ss.Spec.Stages {
		stages[ss.Spec.Stages[i].Name] = true
	}
	for i := range ss.Spec.Migrations {
		m := &ss.Spec.Migrations[i]
		if !stages[m.Stage] {
			return fmt.Errorf("migration %q anchors to unknown stage %q", m.Name, m.Stage)
		}
		for j := range m.Actions {
			if n := actionTypeCount(&m.Actions[j]); n != 1 {
				return fmt.Errorf("migration %q action %q: exactly one verb must be set, found %d", m.Name, m.Actions[j].Name, n)
			}
		}
	}
	return nil
}

// ValidateStages enforces the action oneof invariant: every action sets
// exactly one of patch/http/wait/job/delete. It is shared by both the
// admission webhook and the reconciler fallback (so a StageSet that slips past
// a bypassed/disabled webhook still fails loudly rather than reaching an action
// executor with an undefined verb).
func ValidateStages(ss *stagesv1.StageSet) error {
	for i := range ss.Spec.Stages {
		stage := &ss.Spec.Stages[i]
		if stage.Actions == nil {
			continue
		}
		phases := []struct {
			name    string
			actions []stagesv1.Action
		}{
			{"pre", stage.Actions.Pre},
			{"post", stage.Actions.Post},
			{"onFailure", stage.Actions.OnFailure},
		}
		for _, phase := range phases {
			for j := range phase.actions {
				a := &phase.actions[j]
				if n := actionTypeCount(a); n != 1 {
					return fmt.Errorf("stage %q action %q (%s): exactly one of patch, http, wait, job, delete, apply must be set, found %d", stage.Name, a.Name, phase.name, n)
				}
			}
		}
	}
	return nil
}

func actionTypeCount(a *stagesv1.Action) int {
	n := 0
	if a.Patch != nil {
		n++
	}
	if a.HTTP != nil {
		n++
	}
	if a.Wait != nil {
		n++
	}
	if a.Job != nil {
		n++
	}
	if a.Delete != nil {
		n++
	}
	if a.Apply != nil {
		n++
	}
	return n
}
