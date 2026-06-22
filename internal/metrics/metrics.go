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

	// TeardownForceDropTotal counts StageSets whose finalizer was force-dropped
	// because teardown kept failing past --max-teardown-wait. A force-drop
	// orphans whatever objects the failing stage's Delete could not remove, so
	// sustained non-zero values flag a permanently-unreachable target cluster
	// that operators must clean up by hand. See the teardown runbook.
	TeardownForceDropTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "stageset_teardown_force_drop_total",
		Help: "StageSets whose finalizer was force-dropped after --max-teardown-wait of failing teardown. Sustained non-zero values flag an unreachable target and accumulating orphaned objects.",
	}, []string{"namespace", "name"})

	// InventorySkippedEntriesTotal counts malformed StageInventory entries that
	// could not be parsed back into an ObjectRef. A skipped entry means the
	// object it named escapes pruning forever (the planner never sees it as a
	// stored ref), so sustained non-zero values flag corrupted inventory shards
	// and accumulating un-prunable objects.
	InventorySkippedEntriesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "stageset_inventory_skipped_entries_total",
		Help: "Malformed StageInventory entries skipped during a read. Sustained non-zero values mean named objects escape pruning.",
	}, []string{"namespace", "stageset", "stage"})

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
	ctrlmetrics.Registry.MustRegister(ReconcileTotal, StageAppliedTotal, DriftCorrectedTotal, UpdateDeferredTotal, WebhookCertRenewalFailuresTotal, WatchEngagementFailuresTotal, TeardownForceDropTotal, InventorySkippedEntriesTotal, StageReady)
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
// deleted StageSet does not leave a stale gauge behind.
func DeleteStageReady(namespace, stageset string) {
	StageReady.DeletePartialMatch(prometheus.Labels{"namespace": namespace, "stageset": stageset})
}

// DeleteStageReadyForStage removes the readiness series for a single stage. It
// is called when a stage is dropped from a live StageSet's spec, so a
// metric-based rollout gate doesn't keep reading a phantom stage's last value.
func DeleteStageReadyForStage(namespace, stageset, stage string) {
	StageReady.DeleteLabelValues(namespace, stageset, stage)
}

// DeleteStageSetMetrics evicts every per-StageSet operational time series so a
// deleted StageSet leaves no orphaned series pinned in the registry. Without
// this, a cluster that churns StageSets (CI namespaces, GitOps create/delete
// cycles) grows the controller's resident set and /metrics scrape size without
// bound. The gauge is handled separately by DeleteStageReady; this covers the
// counters and histograms keyed by the StageSet identity.
//
// TeardownForceDropTotal is deliberately NOT evicted: it is emitted at deletion
// as the alert signal that a finalizer was force-dropped with orphaned objects
// left behind, so it must outlive the StageSet — its cardinality is bounded by
// the (rare) force-drop incident count, not by StageSet churn.
func DeleteStageSetMetrics(namespace, name string) {
	nameMatch := prometheus.Labels{"namespace": namespace, "name": name}
	ReconcileTotal.DeletePartialMatch(nameMatch)
	StageAppliedTotal.DeletePartialMatch(nameMatch)
	DriftCorrectedTotal.DeletePartialMatch(nameMatch)
	UpdateDeferredTotal.DeletePartialMatch(nameMatch)
	// InventorySkippedEntriesTotal labels the StageSet as "stageset", not "name".
	InventorySkippedEntriesTotal.DeletePartialMatch(prometheus.Labels{"namespace": namespace, "stageset": name})
}
