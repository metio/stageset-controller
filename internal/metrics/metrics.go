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
)

func init() {
	ctrlmetrics.Registry.MustRegister(ReconcileTotal, StageAppliedTotal, DriftCorrectedTotal, UpdateDeferredTotal, WebhookCertRenewalFailuresTotal)
}
