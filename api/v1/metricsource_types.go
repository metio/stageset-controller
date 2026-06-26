// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package v1

import "github.com/fluxcd/pkg/apis/meta"

// MetricSource resolves to a single scalar at evaluation time. It is the shared
// contract both gate families consume: a rollout-wide error-budget freeze
// (spec.errorBudget) and a per-stage promotion analysis (stage.promotion.analysis).
// The controller carries no SLO logic — it reads one number from the source and
// compares it to a Threshold. Window/budgeting-method (rolling vs calendar,
// occurrences vs timeslices) stay in the source, so the gate is correct for both
// with no code change.
type MetricSource struct {
	// Prometheus runs an instant query that must evaluate to a single scalar.
	// PromQL natively covers Sloth, Pyrra, Grafana, and Nobl9's Prometheus API,
	// so the bulk of SLO installs need no per-tool code. Exactly one of
	// prometheus or webhook must be set.
	// +optional
	Prometheus *PrometheusSource `json:"prometheus,omitempty"`

	// Webhook fetches a JSON document over HTTP and extracts the scalar with a
	// JSONPath expression — the escape hatch for SaaS SLO APIs (Nobl9, Grafana
	// Cloud) that have no Prometheus endpoint. Exactly one of prometheus or
	// webhook must be set.
	// +optional
	Webhook *WebhookSource `json:"webhook,omitempty"`
}

// WebhookSource fetches a JSON document and extracts a single scalar from it.
type WebhookSource struct {
	// URL is the endpoint to GET. Public addresses are expected (SaaS APIs);
	// loopback, link-local (incl. cloud metadata), multicast, and unspecified
	// addresses are refused, mirroring the http-action SSRF guard.
	// +required
	URL string `json:"url"`

	// JSONPath is a kubectl-style JSONPath expression selecting the scalar from
	// the response body, e.g. '{.objectives[0].errorBudgetRemaining}'. It must
	// resolve to exactly one numeric (or numeric-string) value.
	// +required
	JSONPath string `json:"jsonPath"`

	// SecretRef optionally names a Secret in the StageSet's namespace holding a
	// bearer token under the "token" key, sent as Authorization: Bearer.
	// +optional
	SecretRef *meta.LocalObjectReference `json:"secretRef,omitempty"`
}

// PrometheusSource is an instant Prometheus query yielding one scalar.
type PrometheusSource struct {
	// Address is the Prometheus base URL, e.g. http://prometheus.monitoring:9090.
	// In-cluster private addresses are expected and allowed; only loopback,
	// link-local (incl. cloud metadata), multicast, and unspecified addresses are
	// refused, mirroring the http-action SSRF guard.
	// +required
	Address string `json:"address"`

	// Query is an instant PromQL query that MUST evaluate to a single scalar (a
	// scalar result, or an instant vector with exactly one sample). For an
	// error-budget freeze this is typically the remaining budget as a 0..1 ratio
	// (e.g. Sloth's slo:period_error_budget_remaining:ratio{...}).
	// +required
	Query string `json:"query"`

	// SecretRef optionally names a Secret in the StageSet's namespace holding a
	// bearer token under the "token" key, sent as Authorization: Bearer on the
	// query. The Secret is read with the controller's client.
	// +optional
	SecretRef *meta.LocalObjectReference `json:"secretRef,omitempty"`
}

// Threshold bounds the scalar a MetricSource returns. min/max mirror Flagger's
// thresholdRange, so the same shape expresses "must stay above X" and "must stay
// below Y". Both bounds are inclusive; omit a bound for no limit on that side.
// The values are decimal strings (e.g. "0.01") so fractional thresholds stay
// exact in YAML.
type Threshold struct {
	// Min, when set, requires the scalar to be >= this value.
	// +optional
	Min *string `json:"min,omitempty"`

	// Max, when set, requires the scalar to be <= this value.
	// +optional
	Max *string `json:"max,omitempty"`
}
