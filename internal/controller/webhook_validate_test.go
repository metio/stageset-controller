// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"testing"
	"time"

	"github.com/fluxcd/pkg/apis/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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

func okSource() stagesv1.MetricSource {
	return stagesv1.MetricSource{Prometheus: &stagesv1.PrometheusSource{Address: "http://p:9090", Query: "q"}}
}

// TestValidateErrorBudget covers spec.errorBudget shape: a usable source,
// numeric thresholds, resume not below freeze, and a known onSourceError.
func TestValidateErrorBudget(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		eb      *stagesv1.ErrorBudget
		wantErr bool
	}{
		{name: "nil is fine"},
		{name: "minimal valid", eb: &stagesv1.ErrorBudget{Source: okSource(), FreezeThreshold: "0"}},
		{name: "with hysteresis + onSourceError", eb: &stagesv1.ErrorBudget{Source: okSource(), FreezeThreshold: "0", ResumeThreshold: "0.05", OnSourceError: "Hold"}},
		{name: "missing source", eb: &stagesv1.ErrorBudget{FreezeThreshold: "0"}, wantErr: true},
		{name: "missing address", eb: &stagesv1.ErrorBudget{Source: stagesv1.MetricSource{Prometheus: &stagesv1.PrometheusSource{Query: "q"}}, FreezeThreshold: "0"}, wantErr: true},
		{name: "missing query", eb: &stagesv1.ErrorBudget{Source: stagesv1.MetricSource{Prometheus: &stagesv1.PrometheusSource{Address: "http://p:9090"}}, FreezeThreshold: "0"}, wantErr: true},
		{name: "bad freezeThreshold", eb: &stagesv1.ErrorBudget{Source: okSource(), FreezeThreshold: "x"}, wantErr: true},
		{name: "bad resumeThreshold", eb: &stagesv1.ErrorBudget{Source: okSource(), FreezeThreshold: "0", ResumeThreshold: "y"}, wantErr: true},
		{name: "resume below freeze", eb: &stagesv1.ErrorBudget{Source: okSource(), FreezeThreshold: "0.1", ResumeThreshold: "0.05"}, wantErr: true},
		{name: "unknown onSourceError", eb: &stagesv1.ErrorBudget{Source: okSource(), FreezeThreshold: "0", OnSourceError: "Maybe"}, wantErr: true},
		{name: "secretRef without name", eb: &stagesv1.ErrorBudget{Source: stagesv1.MetricSource{Prometheus: &stagesv1.PrometheusSource{Address: "http://p:9090", Query: "q", SecretRef: &meta.LocalObjectReference{}}}, FreezeThreshold: "0"}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ss := &stagesv1.StageSet{}
			ss.Spec.ErrorBudget = tc.eb
			if err := validateErrorBudget(ss); (err != nil) != tc.wantErr {
				t.Fatalf("validateErrorBudget err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

// TestValidatePromotion covers stage.promotion.analysis shape: at least one
// check, named unique checks, a usable source, a threshold bound, and known
// onFailure/onSourceError values.
func TestValidatePromotion(t *testing.T) {
	t.Parallel()
	max := "0.01"
	okCheck := stagesv1.AnalysisCheck{Name: "err", Source: okSource(), Threshold: stagesv1.Threshold{Max: &max}}
	tests := []struct {
		name    string
		an      *stagesv1.PromotionAnalysis
		wantErr bool
	}{
		{name: "nil analysis is fine"},
		{name: "minimal valid", an: &stagesv1.PromotionAnalysis{Checks: []stagesv1.AnalysisCheck{okCheck}}},
		{name: "valid with policies", an: &stagesv1.PromotionAnalysis{Checks: []stagesv1.AnalysisCheck{okCheck}, OnFailure: "Rollback", OnSourceError: "Allow"}},
		{name: "no checks", an: &stagesv1.PromotionAnalysis{}, wantErr: true},
		{name: "empty check name", an: &stagesv1.PromotionAnalysis{Checks: []stagesv1.AnalysisCheck{{Source: okSource(), Threshold: stagesv1.Threshold{Max: &max}}}}, wantErr: true},
		{name: "duplicate check name", an: &stagesv1.PromotionAnalysis{Checks: []stagesv1.AnalysisCheck{okCheck, okCheck}}, wantErr: true},
		{name: "check missing source", an: &stagesv1.PromotionAnalysis{Checks: []stagesv1.AnalysisCheck{{Name: "x", Threshold: stagesv1.Threshold{Max: &max}}}}, wantErr: true},
		{name: "check with no threshold bound", an: &stagesv1.PromotionAnalysis{Checks: []stagesv1.AnalysisCheck{{Name: "x", Source: okSource()}}}, wantErr: true},
		{name: "bad onFailure", an: &stagesv1.PromotionAnalysis{Checks: []stagesv1.AnalysisCheck{okCheck}, OnFailure: "Nuke"}, wantErr: true},
		{name: "bad onSourceError", an: &stagesv1.PromotionAnalysis{Checks: []stagesv1.AnalysisCheck{okCheck}, OnSourceError: "Maybe"}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ss := &stagesv1.StageSet{}
			ss.Spec.Stages = []stagesv1.Stage{{Name: "staging", Promotion: &stagesv1.StagePromotion{Analysis: tc.an}}}
			if err := validatePromotion(ss); (err != nil) != tc.wantErr {
				t.Fatalf("validatePromotion err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func webhookSource() stagesv1.MetricSource {
	return stagesv1.MetricSource{Webhook: &stagesv1.WebhookSource{URL: "https://slo.example/api", JSONPath: "{.remaining}"}}
}

// TestValidateMetricSource_Union covers the prometheus|webhook oneof and the
// per-provider field checks.
func TestValidateMetricSource_Union(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		src     stagesv1.MetricSource
		wantErr bool
	}{
		{"prometheus ok", okSource(), false},
		{"webhook ok", webhookSource(), false},
		{"neither set", stagesv1.MetricSource{}, true},
		{"both set", stagesv1.MetricSource{Prometheus: okSource().Prometheus, Webhook: webhookSource().Webhook}, true},
		{"webhook missing url", stagesv1.MetricSource{Webhook: &stagesv1.WebhookSource{JSONPath: "{.x}"}}, true},
		{"webhook missing jsonPath", stagesv1.MetricSource{Webhook: &stagesv1.WebhookSource{URL: "https://x"}}, true},
		{"webhook secretRef no name", stagesv1.MetricSource{Webhook: &stagesv1.WebhookSource{URL: "https://x", JSONPath: "{.x}", SecretRef: &meta.LocalObjectReference{}}}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := validateMetricSource("src", tc.src); (err != nil) != tc.wantErr {
				t.Fatalf("validateMetricSource err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

// TestValidatePromotion_FastTrack covers fast-track's shape: it requires a soak,
// a usable source, and a numeric max.
func TestValidatePromotion_FastTrack(t *testing.T) {
	t.Parallel()
	soak := &metav1.Duration{Duration: time.Minute}
	tests := []struct {
		name    string
		promo   *stagesv1.StagePromotion
		wantErr bool
	}{
		{"fastTrack with soak ok", &stagesv1.StagePromotion{Soak: soak, FastTrack: &stagesv1.FastTrack{Source: okSource(), Max: "1"}}, false},
		{"fastTrack without soak rejected", &stagesv1.StagePromotion{FastTrack: &stagesv1.FastTrack{Source: okSource(), Max: "1"}}, true},
		{"fastTrack bad source", &stagesv1.StagePromotion{Soak: soak, FastTrack: &stagesv1.FastTrack{Source: stagesv1.MetricSource{}, Max: "1"}}, true},
		{"fastTrack bad max", &stagesv1.StagePromotion{Soak: soak, FastTrack: &stagesv1.FastTrack{Source: okSource(), Max: "fast"}}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ss := &stagesv1.StageSet{}
			ss.Spec.Stages = []stagesv1.Stage{{Name: "s", Promotion: tc.promo}}
			if err := validatePromotion(ss); (err != nil) != tc.wantErr {
				t.Fatalf("validatePromotion err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

// TestValidatePromotion_RestartGate covers restart-gate shape: a known onFailure
// (gate and per-check), at least one check, and each check a unique name with a
// non-empty selector.
func TestValidatePromotion_RestartGate(t *testing.T) {
	t.Parallel()
	sel := metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}}
	check := func(name string, s metav1.LabelSelector) stagesv1.RestartCheck {
		return stagesv1.RestartCheck{Name: name, Selector: s}
	}
	gate := func(onFailure string, checks ...stagesv1.RestartCheck) *stagesv1.RestartGate {
		return &stagesv1.RestartGate{OnFailure: onFailure, Checks: checks}
	}
	tests := []struct {
		name    string
		gate    *stagesv1.RestartGate
		wantErr bool
	}{
		{"valid", gate("", check("api", sel)), false},
		{"valid with gate onFailure", gate("Rollback", check("api", sel)), false},
		{"empty name", gate("", check("", sel)), true},
		{"empty selector", gate("", check("api", metav1.LabelSelector{})), true},
		{"duplicate name", gate("", check("api", sel), check("api", sel)), true},
		{"no checks", gate("Hold"), true},
		{"bad gate onFailure", gate("Nope", check("api", sel)), true},
		{"bad check onFailure", &stagesv1.RestartGate{Checks: []stagesv1.RestartCheck{{Name: "api", Selector: sel, OnFailure: "Nope"}}}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ss := &stagesv1.StageSet{}
			ss.Spec.Stages = []stagesv1.Stage{{Name: "s", Promotion: &stagesv1.StagePromotion{RestartGate: tc.gate}}}
			if err := validatePromotion(ss); (err != nil) != tc.wantErr {
				t.Fatalf("validatePromotion err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

// TestValidatePromotion_EventGate covers event-gate shape: known onFailure (gate
// and per-check), at least one check, and each check a unique name with a
// non-empty selector and at least one reason.
func TestValidatePromotion_EventGate(t *testing.T) {
	t.Parallel()
	sel := metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}}
	check := func(name string, s metav1.LabelSelector, reasons ...string) stagesv1.EventCheck {
		return stagesv1.EventCheck{Name: name, Selector: s, Reasons: reasons}
	}
	gate := func(onFailure string, checks ...stagesv1.EventCheck) *stagesv1.EventGate {
		return &stagesv1.EventGate{OnFailure: onFailure, Checks: checks}
	}
	tests := []struct {
		name    string
		gate    *stagesv1.EventGate
		wantErr bool
	}{
		{"valid", gate("", check("api", sel, "FailedScheduling")), false},
		{"valid with gate onFailure", gate("Rollback", check("api", sel, "OOMKilling")), false},
		{"empty name", gate("", check("", sel, "OOMKilling")), true},
		{"empty selector", gate("", check("api", metav1.LabelSelector{}, "OOMKilling")), true},
		{"no reasons", gate("", check("api", sel)), true},
		{"duplicate name", gate("", check("api", sel, "OOMKilling"), check("api", sel, "OOMKilling")), true},
		{"no checks", gate("Hold"), true},
		{"bad gate onFailure", gate("Nope", check("api", sel, "OOMKilling")), true},
		{"bad check onFailure", &stagesv1.EventGate{Checks: []stagesv1.EventCheck{{Name: "api", Selector: sel, Reasons: []string{"OOMKilling"}, OnFailure: "Nope"}}}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ss := &stagesv1.StageSet{}
			ss.Spec.Stages = []stagesv1.Stage{{Name: "s", Promotion: &stagesv1.StagePromotion{EventGate: tc.gate}}}
			if err := validatePromotion(ss); (err != nil) != tc.wantErr {
				t.Fatalf("validatePromotion err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

// TestValidateErrorBudget_PerStage covers that a stage's own errorBudget is
// validated like the rollout-wide one.
func TestValidateErrorBudget_PerStage(t *testing.T) {
	t.Parallel()
	ss := &stagesv1.StageSet{}
	ss.Spec.Stages = []stagesv1.Stage{{Name: "prod", ErrorBudget: &stagesv1.ErrorBudget{Source: webhookSource(), FreezeThreshold: "0.1"}}}
	if err := validateErrorBudget(ss); err != nil {
		t.Fatalf("valid per-stage budget rejected: %v", err)
	}
	ss.Spec.Stages[0].ErrorBudget.ResumeThreshold = "0.05" // < freeze
	if err := validateErrorBudget(ss); err == nil {
		t.Fatal("per-stage resume < freeze should be rejected")
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
			name: "cross-namespace source rejected",
			ss: spec(stagesv1.StageSetSpec{
				Version: ver, Stages: []stagesv1.Stage{{Name: "deploy"}},
				MigrationsSourceRef: &stagesv1.MigrationsSource{SourceRef: stagesv1.SourceReference{Name: "ladder", Namespace: "other"}},
			}),
			wantErr: true,
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
