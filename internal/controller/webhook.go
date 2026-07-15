// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"fmt"
	"reflect"
	"strconv"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/artifact"
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
//
// It skips re-validation in two cases so a spec-unchanged update is never
// blocked by a violation the update does not introduce:
//   - the object is being deleted (a finalizer-removal update must not be
//     denied, or the StageSet wedges in Terminating); and
//   - the validation-relevant spec is unchanged (spec.suspend is normalized out
//     because the webhook does not validate it). Because ValidateSpec is a pure
//     function of the spec, a StageSet can only become retroactively invalid
//     across an operator/CRD upgrade that tightens a check (or a create that
//     bypassed the webhook). Without this skip, every later update to such an
//     object — including `kubectl patch spec.suspend=true`, a `flux reconcile`
//     annotation, and the MCP suspend/resume/reconcile tools — would be denied,
//     blocking the very remediations meant to pause or unstick it.
func (v *StageSetValidator) ValidateUpdate(_ context.Context, oldObj, newObj *stagesv1.StageSet) (admission.Warnings, error) {
	if !newObj.GetDeletionTimestamp().IsZero() {
		return nil, nil
	}
	if oldObj != nil && validationInputsUnchanged(oldObj, newObj) {
		return nil, nil
	}
	return nil, ValidateSpec(newObj)
}

// validationInputsUnchanged reports whether the spec fields ValidateSpec checks
// are identical between old and new. spec.suspend is normalized out — the
// webhook does not validate it, so a pause/resume toggle must count as an
// unchanged input. A metadata-only update (e.g. a reconcile annotation) leaves
// the whole spec identical and is likewise treated as unchanged.
func validationInputsUnchanged(oldObj, newObj *stagesv1.StageSet) bool {
	a := oldObj.Spec
	b := newObj.Spec
	a.Suspend = b.Suspend
	return reflect.DeepEqual(a, b)
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
	if err := validateErrorBudget(ss); err != nil {
		return err
	}
	if err := validatePromotion(ss); err != nil {
		return err
	}
	if err := validateReadyChecks(ss); err != nil {
		return err
	}
	return window.Validate(ss.Spec.UpdateWindows)
}

// validateReadyChecks rejects malformed stage readiness checks at admission: a
// CEL ReadyChecks.Exprs that doesn't compile, and a ReadyChecks.Checks
// reference missing its kind or name. Without this the reconciler would accept
// them and then fail (or silently no-op) only at apply time.
func validateReadyChecks(ss *stagesv1.StageSet) error {
	for i := range ss.Spec.Stages {
		stage := &ss.Spec.Stages[i]
		if stage.ReadyChecks == nil {
			continue
		}
		for j, ref := range stage.ReadyChecks.Checks {
			if ref.Kind == "" || ref.Name == "" {
				return fmt.Errorf("stage %q readyChecks.checks[%d]: kind and name are required", stage.Name, j)
			}
		}
		if _, err := compileReadyExprs(stage); err != nil {
			return fmt.Errorf("stage %q %w", stage.Name, err)
		}
	}
	return nil
}

// validateErrorBudget enforces the shape of the rollout-wide spec.errorBudget
// and each stage's per-stage errorBudget: a usable metric source, numeric
// thresholds, a resumeThreshold not below freezeThreshold (so hysteresis can't
// invert), and a known onSourceError value.
func validateErrorBudget(ss *stagesv1.StageSet) error {
	if err := validateBudget("spec.errorBudget", ss.Spec.ErrorBudget); err != nil {
		return err
	}
	for i := range ss.Spec.Stages {
		st := &ss.Spec.Stages[i]
		if err := validateBudget(fmt.Sprintf("stage %q errorBudget", st.Name), st.ErrorBudget); err != nil {
			return err
		}
	}
	return nil
}

func validateBudget(where string, eb *stagesv1.ErrorBudget) error {
	if eb == nil {
		return nil
	}
	if err := validateMetricSource(where+".source", eb.Source); err != nil {
		return err
	}
	freeze, err := parseThresholdValue(where+".freezeThreshold", eb.FreezeThreshold)
	if err != nil {
		return err
	}
	if eb.ResumeThreshold != "" {
		resume, err := parseThresholdValue(where+".resumeThreshold", eb.ResumeThreshold)
		if err != nil {
			return err
		}
		if resume < freeze {
			return fmt.Errorf("%s.resumeThreshold (%s) must be >= freezeThreshold (%s)", where, eb.ResumeThreshold, eb.FreezeThreshold)
		}
	}
	switch eb.OnSourceError {
	case "", "Allow", "Hold":
	default:
		return fmt.Errorf("%s.onSourceError must be Allow or Hold, got %q", where, eb.OnSourceError)
	}
	return nil
}

// validatePromotion enforces stage.promotion.analysis shape across all stages:
// each analysis check needs a name, a usable source, and well-formed thresholds;
// onFailure/onSourceError must be known values.
func validatePromotion(ss *stagesv1.StageSet) error {
	for i := range ss.Spec.Stages {
		st := &ss.Spec.Stages[i]
		if st.Promotion == nil {
			continue
		}
		if ft := st.Promotion.FastTrack; ft != nil {
			if st.Promotion.Soak == nil || st.Promotion.Soak.Duration <= 0 {
				return fmt.Errorf("stage %q promotion.fastTrack requires a soak to shorten", st.Name)
			}
			if err := validateMetricSource(fmt.Sprintf("stage %q promotion.fastTrack.source", st.Name), ft.Source); err != nil {
				return err
			}
			if _, err := parseThresholdValue(fmt.Sprintf("stage %q promotion.fastTrack.max", st.Name), ft.Max); err != nil {
				return err
			}
		}
		if rg := st.Promotion.RestartGate; rg != nil {
			switch rg.OnFailure {
			case "", "Hold", "Rollback":
			default:
				return fmt.Errorf("stage %q promotion.restartGate.onFailure must be Hold or Rollback, got %q", st.Name, rg.OnFailure)
			}
			if len(rg.Checks) == 0 {
				return fmt.Errorf("stage %q promotion.restartGate must declare at least one check", st.Name)
			}
			rcNames := map[string]bool{}
			for j := range rg.Checks {
				c := &rg.Checks[j]
				if c.Name == "" {
					return fmt.Errorf("stage %q promotion.restartGate has a check with an empty name", st.Name)
				}
				if rcNames[c.Name] {
					return fmt.Errorf("stage %q promotion.restartGate has duplicate check name %q", st.Name, c.Name)
				}
				rcNames[c.Name] = true
				sel, err := metav1.LabelSelectorAsSelector(&c.Selector)
				if err != nil {
					return fmt.Errorf("stage %q promotion.restartGate check %q has an invalid selector: %w", st.Name, c.Name, err)
				}
				if sel.Empty() {
					return fmt.Errorf("stage %q promotion.restartGate check %q selector must match at least one label", st.Name, c.Name)
				}
				switch c.OnFailure {
				case "", "Hold", "Rollback":
				default:
					return fmt.Errorf("stage %q promotion.restartGate check %q onFailure must be Hold or Rollback, got %q", st.Name, c.Name, c.OnFailure)
				}
			}
		}
		if eg := st.Promotion.EventGate; eg != nil {
			switch eg.OnFailure {
			case "", "Hold", "Rollback":
			default:
				return fmt.Errorf("stage %q promotion.eventGate.onFailure must be Hold or Rollback, got %q", st.Name, eg.OnFailure)
			}
			if len(eg.Checks) == 0 {
				return fmt.Errorf("stage %q promotion.eventGate must declare at least one check", st.Name)
			}
			ecNames := map[string]bool{}
			for j := range eg.Checks {
				c := &eg.Checks[j]
				if c.Name == "" {
					return fmt.Errorf("stage %q promotion.eventGate has a check with an empty name", st.Name)
				}
				if ecNames[c.Name] {
					return fmt.Errorf("stage %q promotion.eventGate has duplicate check name %q", st.Name, c.Name)
				}
				ecNames[c.Name] = true
				sel, err := metav1.LabelSelectorAsSelector(&c.Selector)
				if err != nil {
					return fmt.Errorf("stage %q promotion.eventGate check %q has an invalid selector: %w", st.Name, c.Name, err)
				}
				if sel.Empty() {
					return fmt.Errorf("stage %q promotion.eventGate check %q selector must match at least one label", st.Name, c.Name)
				}
				if len(c.Reasons) == 0 {
					return fmt.Errorf("stage %q promotion.eventGate check %q must name at least one event reason", st.Name, c.Name)
				}
				switch c.OnFailure {
				case "", "Hold", "Rollback":
				default:
					return fmt.Errorf("stage %q promotion.eventGate check %q onFailure must be Hold or Rollback, got %q", st.Name, c.Name, c.OnFailure)
				}
			}
		}
		if st.Promotion.Analysis == nil {
			continue
		}
		an := st.Promotion.Analysis
		if len(an.Checks) == 0 {
			return fmt.Errorf("stage %q promotion.analysis must declare at least one check", st.Name)
		}
		names := map[string]bool{}
		for j := range an.Checks {
			c := &an.Checks[j]
			if c.Name == "" {
				return fmt.Errorf("stage %q promotion.analysis has a check with an empty name", st.Name)
			}
			if names[c.Name] {
				return fmt.Errorf("stage %q promotion.analysis has duplicate check name %q", st.Name, c.Name)
			}
			names[c.Name] = true
			where := fmt.Sprintf("stage %q promotion.analysis check %q source", st.Name, c.Name)
			if err := validateMetricSource(where, c.Source); err != nil {
				return err
			}
			if err := validateThresholdBounds(st.Name, c); err != nil {
				return err
			}
		}
		switch an.OnFailure {
		case "", "Hold", "Rollback":
		default:
			return fmt.Errorf("stage %q promotion.analysis.onFailure must be Hold or Rollback, got %q", st.Name, an.OnFailure)
		}
		switch an.OnSourceError {
		case "", "Hold", "Allow":
		default:
			return fmt.Errorf("stage %q promotion.analysis.onSourceError must be Hold or Allow, got %q", st.Name, an.OnSourceError)
		}
	}
	return nil
}

func validateThresholdBounds(stage string, c *stagesv1.AnalysisCheck) error {
	if c.Threshold.Min == nil && c.Threshold.Max == nil {
		return fmt.Errorf("stage %q promotion.analysis check %q must set at least one of threshold.min or threshold.max", stage, c.Name)
	}
	if c.Threshold.Min != nil {
		if _, err := parseThresholdValue(fmt.Sprintf("stage %q check %q threshold.min", stage, c.Name), *c.Threshold.Min); err != nil {
			return err
		}
	}
	if c.Threshold.Max != nil {
		if _, err := parseThresholdValue(fmt.Sprintf("stage %q check %q threshold.max", stage, c.Name), *c.Threshold.Max); err != nil {
			return err
		}
	}
	return nil
}

// validateMetricSource enforces that a MetricSource names exactly one usable
// provider (prometheus or webhook).
func validateMetricSource(where string, src stagesv1.MetricSource) error {
	n := 0
	if src.Prometheus != nil {
		n++
	}
	if src.Webhook != nil {
		n++
	}
	if n != 1 {
		return fmt.Errorf("%s must set exactly one of prometheus or webhook, found %d", where, n)
	}
	if p := src.Prometheus; p != nil {
		if p.Address == "" {
			return fmt.Errorf("%s.prometheus.address must be set", where)
		}
		if p.Query == "" {
			return fmt.Errorf("%s.prometheus.query must be set", where)
		}
		if p.SecretRef != nil && p.SecretRef.Name == "" {
			return fmt.Errorf("%s.prometheus.secretRef.name must be set when secretRef is given", where)
		}
	}
	if w := src.Webhook; w != nil {
		if w.URL == "" {
			return fmt.Errorf("%s.webhook.url must be set", where)
		}
		if w.JSONPath == "" {
			return fmt.Errorf("%s.webhook.jsonPath must be set", where)
		}
		if w.SecretRef != nil && w.SecretRef.Name == "" {
			return fmt.Errorf("%s.webhook.secretRef.name must be set when secretRef is given", where)
		}
	}
	return nil
}

func parseThresholdValue(where, s string) (float64, error) {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("%s %q is not a number", where, s)
	}
	return v, nil
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

	// Migration names are the idempotency-ledger key, so the inline ladder must
	// have unique names (and stay within the per-ladder limit) — the same check a
	// sourced ladder gets in resolveMigrationLadder. Without it, two migrations
	// sharing a name let pruneSupersededLedger drop a sibling entry, and that
	// migration's destructive actions re-execute on the next reconcile.
	// ValidateLadder also runs ValidateMigration on each entry.
	if err := migrations.ValidateLadder(ss.Spec.Migrations); err != nil {
		return err
	}
	for i := range ss.Spec.Migrations {
		m := &ss.Spec.Migrations[i]
		if m.Stage != "" && !anchors[m.Stage] {
			return fmt.Errorf("migration %q anchors to unknown stage or anchor %q", m.Name, m.Stage)
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
		// A producer-kind sourceRef (e.g. JsonnetSnippet) is matched against
		// ExternalArtifact back-pointers by API GROUP; without apiVersion the
		// group is empty and can never match a real producer's back-pointer —
		// the reference is unresolvable by construction, so reject it here
		// with the actionable message instead of a misleading not-found later.
		if ref := ss.Spec.Stages[i].SourceRef; artifact.IsProducerRef(ref) && ref.APIVersion == "" {
			return fmt.Errorf("stage %q sourceRef: kind %q is a producer reference and requires apiVersion (the API group identifies the producer's ExternalArtifact back-pointer)", ss.Spec.Stages[i].Name, ref.Kind)
		}
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
	// spec.onRollback is a StageSet-level action list, ledger-free, so it has its
	// own namespace of names — validate the oneof verb and name uniqueness within
	// the list, independent of the per-stage phases above.
	rbSeen := make(map[string]bool, len(ss.Spec.OnRollback))
	for i := range ss.Spec.OnRollback {
		a := &ss.Spec.OnRollback[i]
		if n := a.VerbCount(); n != 1 {
			return fmt.Errorf("onRollback action %q: exactly one of patch, http, wait, job, delete, apply must be set, found %d", a.Name, n)
		}
		if a.Name == "" {
			return fmt.Errorf("onRollback has an action with an empty name; action names identify the action in events and must be set")
		}
		if rbSeen[a.Name] {
			return fmt.Errorf("onRollback has duplicate action name %q; action names must be unique", a.Name)
		}
		rbSeen[a.Name] = true
	}
	return nil
}
