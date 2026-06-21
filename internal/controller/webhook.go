// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"fmt"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/migrations"
	"github.com/metio/stageset-controller/internal/window"
)

// +kubebuilder:webhook:path=/validate-stages-metio-wtf-v1-stageset,mutating=false,failurePolicy=fail,sideEffects=None,groups=stages.metio.wtf,resources=stagesets,verbs=create;update,versions=v1,name=vstageset.stages.metio.wtf,admissionReviewVersions=v1

// StageSetValidator is the validating admission webhook for StageSet. It
// enforces invariants the CRD schema cannot express cheaply — currently that
// every action sets exactly one of patch/http/wait/job/delete/apply. Keeping this out of a
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
	if err := validateKubeConfig(ss); err != nil {
		return err
	}
	return window.Validate(ss.Spec.UpdateWindows)
}

// validateKubeConfig checks spec.kubeConfig at the level admission can see. A
// secretRef names a self-contained kubeconfig; a configMapRef selects
// cloud-provider workload-identity auth (AWS / Azure / GCP / generic). Exactly
// one must be set, and each reference must carry a name.
//
// The configMap's contents — its provider name and per-provider keys — aren't
// readable at admission (the webhook only sees the StageSet), so that
// validation happens at reconcile time when the ConfigMap is read, surfacing as
// a terminal ReasonInvalidSpec. This guard only enforces the shape.
func validateKubeConfig(ss *stagesv1.StageSet) error {
	kc := ss.Spec.KubeConfig
	if kc == nil {
		return nil
	}
	hasSecret := kc.SecretRef != nil
	hasConfigMap := kc.ConfigMapRef != nil
	switch {
	case hasSecret && hasConfigMap:
		return fmt.Errorf("spec.kubeConfig must set exactly one of secretRef or configMapRef, not both")
	case !hasSecret && !hasConfigMap:
		return fmt.Errorf("spec.kubeConfig must set one of secretRef or configMapRef")
	case hasSecret && kc.SecretRef.Name == "":
		return fmt.Errorf("spec.kubeConfig.secretRef.name must be set")
	case hasConfigMap && kc.ConfigMapRef.Name == "":
		return fmt.Errorf("spec.kubeConfig.configMapRef.name must be set")
	}
	return nil
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

// validateMigrations enforces migration invariants checkable at admission:
// migrations require a version; the inline list and a source ref are mutually
// exclusive; stage anchor keys (Name plus MigrationAnchor) are unique so a
// migration resolves to exactly one stage; and each INLINE migration anchors to
// a known stage/anchor (or, when empty, the first stage) with well-formed
// actions. A SOURCED ladder's entries aren't available here — they are validated
// at fetch time (reason MigrationArtifactInvalid).
func validateMigrations(ss *stagesv1.StageSet) error {
	hasInline := len(ss.Spec.Migrations) > 0
	hasSource := ss.Spec.MigrationsSourceRef != nil
	if hasInline && hasSource {
		return fmt.Errorf("spec.migrations and spec.migrationsSourceRef are mutually exclusive")
	}
	if !hasInline && !hasSource {
		return nil
	}
	if ss.Spec.Version == nil {
		return fmt.Errorf("migrations require spec.version")
	}
	if hasSource && isCrossNamespaceRef(ss.Spec.MigrationsSourceRef.SourceRef, ss.Namespace) {
		return fmt.Errorf("spec.migrationsSourceRef must reference a source in the StageSet's own namespace; cross-namespace migration sources are not allowed")
	}

	// Anchor keys = stage Names plus declared MigrationAnchors; they must be
	// unique across the union so a migration's Stage value resolves to exactly
	// one stage (by anchor preferred, else name).
	anchors := make(map[string]bool, len(ss.Spec.Stages))
	for i := range ss.Spec.Stages {
		st := &ss.Spec.Stages[i]
		if anchors[st.Name] {
			return fmt.Errorf("stage name %q collides with another stage name or migrationAnchor", st.Name)
		}
		anchors[st.Name] = true
		if st.MigrationAnchor != "" {
			if anchors[st.MigrationAnchor] {
				return fmt.Errorf("stage %q migrationAnchor %q collides with another stage name or migrationAnchor", st.Name, st.MigrationAnchor)
			}
			anchors[st.MigrationAnchor] = true
		}
	}

	// A sourced ladder is fetched and validated at reconcile time.
	if hasSource {
		return nil
	}

	for i := range ss.Spec.Migrations {
		m := &ss.Spec.Migrations[i]
		if m.Stage != "" && !anchors[m.Stage] {
			return fmt.Errorf("migration %q anchors to unknown stage or anchor %q", m.Name, m.Stage)
		}
		if err := migrations.ValidateMigration(m); err != nil {
			return err
		}
	}
	return nil
}

// ValidateStages enforces the action oneof invariant: every action sets
// exactly one of patch/http/wait/job/delete/apply. It is shared by both the
// admission webhook and the reconciler fallback (so a StageSet that slips past
// a bypassed/disabled webhook still fails loudly rather than reaching an action
// executor with an undefined verb).
func ValidateStages(ss *stagesv1.StageSet) error {
	// Stage names must be unique within the StageSet: each stamps a
	// stages.metio.wtf/stage label and owns an inventory shard keyed by name, so
	// two stages sharing a name collide — the second clobbers the first's status
	// and the first's objects are never recorded under their own stage (orphaned,
	// never pruned). The CRD enforces only MinItems, and validateMigrations'
	// uniqueness check is skipped for a migration-free StageSet, so enforce it
	// here, where both admission and the reconciler fallback run.
	seen := make(map[string]bool, len(ss.Spec.Stages))
	for i := range ss.Spec.Stages {
		name := ss.Spec.Stages[i].Name
		if seen[name] {
			return fmt.Errorf("stage name %q is not unique; stage names must be unique within the StageSet", name)
		}
		seen[name] = true
	}
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
		// The stage's pre/post/onFailure share one idempotency ledger keyed by
		// action name (a recorded name is skipped on retry), so a name that is
		// empty or repeated across the three phases would silently skip the
		// second action. Reject both up front.
		seen := map[string]string{} // name -> phase it first appeared in
		for _, phase := range phases {
			for j := range phase.actions {
				a := &phase.actions[j]
				if n := a.VerbCount(); n != 1 {
					return fmt.Errorf("stage %q action %q (%s): exactly one of patch, http, wait, job, delete, apply must be set, found %d", stage.Name, a.Name, phase.name, n)
				}
				if a.Name == "" {
					return fmt.Errorf("stage %q has an action with an empty name in %s; action names are the idempotency-ledger key and must be set", stage.Name, phase.name)
				}
				if first, dup := seen[a.Name]; dup {
					return fmt.Errorf("stage %q has duplicate action name %q (%s and %s); action names are the idempotency-ledger key and must be unique across pre, post, and onFailure", stage.Name, a.Name, first, phase.name)
				}
				seen[a.Name] = phase.name
			}
		}
	}
	return nil
}
