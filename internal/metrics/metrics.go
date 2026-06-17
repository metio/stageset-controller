// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

// Package metrics defines the controller's Prometheus metrics, registered
// against controller-runtime's registry so they ride the manager's metrics
// endpoint.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// ReconcileTotal counts reconciles by their terminal Ready reason.
	ReconcileTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "stageset_reconcile_total",
		Help: "Total StageSet reconciles by terminal Ready-condition reason.",
	}, []string{"namespace", "name", "reason"})

	// StageAppliedTotal counts stages that applied and verified successfully.
	StageAppliedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "stageset_stage_applied_total",
		Help: "Total stages applied and verified.",
	}, []string{"namespace", "name", "stage"})

	// DriftCorrectedTotal counts objects whose out-of-band drift the apply
	// corrected on a steady-state reconcile (same revision as last applied).
	DriftCorrectedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "stageset_drift_corrected_total",
		Help: "Total objects whose out-of-band drift was corrected on a steady-state reconcile.",
	}, []string{"namespace", "name", "stage"})

	// UpdateDeferredTotal counts reconciles that held a rollout because an
	// update window was closed.
	UpdateDeferredTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "stageset_update_deferred_total",
		Help: "Total reconciles that deferred a rollout due to a closed update window.",
	}, []string{"namespace", "name"})

	// WebhookCertRenewalFailuresTotal counts failed self-signed webhook cert
	// renewals in the background renewer goroutine.
	WebhookCertRenewalFailuresTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "stageset_webhook_cert_renewal_failures_total",
		Help: "Total failed self-signed webhook certificate renewals.",
	})

	// WatchEngagementFailuresTotal counts failures to engage a dynamic producer
	// watch via Controller.Watch. The watch is engaged lazily the first time a
	// StageSet references a producer kind; a failed engagement is otherwise
	// silent — dependent StageSets stop re-triggering on that producer's
	// upstream changes with no CR-level signal. Sustained non-zero values on a
	// gvk mean StageSets referencing that producer kind won't re-trigger on
	// upstream changes until the watch engages.
	WatchEngagementFailuresTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "stageset_watch_engagement_failures_total",
		Help: "Failures to engage a dynamic producer watch. Sustained non-zero values on a gvk mean dependent StageSets won't re-trigger on that producer kind's upstream changes until the watch engages.",
	}, []string{"gvk"})

	// StageReady reports whether each stage is currently Ready (1) or not (0).
	// A progressive-delivery controller that gates on metrics rather than a
	// webhook — e.g. Argo Rollouts' Prometheus metric provider — can hold a
	// rollout directly on this gauge.
	StageReady = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "stageset_stage_ready",
		Help: "Whether a stage is Ready (1) or not (0) at the current observed state.",
	}, []string{"namespace", "stageset", "stage"})
)

func init() {
	ctrlmetrics.Registry.MustRegister(ReconcileTotal, StageAppliedTotal, DriftCorrectedTotal, UpdateDeferredTotal, WebhookCertRenewalFailuresTotal, WatchEngagementFailuresTotal, StageReady)
}

// SetStageReady publishes the readiness gauge for one stage.
func SetStageReady(namespace, stageset, stage string, ready bool) {
	v := 0.0
	if ready {
		v = 1
	}
	StageReady.WithLabelValues(namespace, stageset, stage).Set(v)
}

// DeleteStageReady removes every stage-readiness series for a StageSet, so a
// deleted StageSet (or a stage dropped from its spec) does not leave a stale
// gauge behind.
func DeleteStageReady(namespace, stageset string) {
	StageReady.DeletePartialMatch(prometheus.Labels{"namespace": namespace, "stageset": stageset})
}
