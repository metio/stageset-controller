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

	// ActionRunsTotal counts pre/post actions actually executed, by ledger scope
	// (Revision or Version). A Version rate well below the Revision rate is the
	// signal that version scoping is holding upgrade choreography off config
	// churn. Deliberately not labeled by action name — that is unbounded at
	// fleet scale; status.stages[].executed*Actions carries the per-action view.
	ActionRunsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "stageset_action_runs_total",
		Help: "Total pre/post actions executed, by ledger scope.",
	}, []string{"namespace", "name", "scope"})

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

	// StagePromotionPending is 1 while a stage is applied and healthy but held by
	// its promotion gate (soaking or awaiting a manual promote), 0 once promoted
	// or when the stage has no gate. A sustained 1 is the alertable "a rollout is
	// parked waiting to advance" signal.
	StagePromotionPending = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "stageset_stage_promotion_pending",
		Help: "Whether a stage is held by its promotion gate (1) or not (0).",
	}, []string{"namespace", "stageset", "stage"})

	// StagePromotionBlocked is 1 while a stage's promotion analysis is failing
	// (breached its thresholds past failureLimit), 0 otherwise. Distinct from
	// StagePromotionPending (which also covers a healthy soak or manual hold):
	// this fires only on a metric-analysis failure.
	StagePromotionBlocked = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "stageset_stage_promotion_blocked",
		Help: "Whether a stage's promotion analysis is failing (1) or not (0).",
	}, []string{"namespace", "stageset", "stage"})

	// BudgetRemaining reports the last observed error-budget scalar for a
	// StageSet (typically a 0..1 ratio). A saturation/budget alert reads this; a
	// value that never matches what the operator expects flags a wrong query.
	BudgetRemaining = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "stageset_budget_remaining",
		Help: "Last observed remaining error budget for a StageSet (typically a 0..1 ratio).",
	}, []string{"namespace", "name"})

	// BudgetFrozen is 1 while an error-budget freeze holds new-revision rollouts
	// (0 under dryRun, which records but does not enforce). A sustained 1 is the
	// alertable "deploys are frozen out of budget" signal.
	BudgetFrozen = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "stageset_budget_frozen",
		Help: "Whether an error-budget freeze is holding rollouts (1) or not (0).",
	}, []string{"namespace", "name"})

	// MetricSourceErrorsTotal counts metric-source query failures across both
	// gate families (error-budget freeze and promotion analysis). Sustained
	// non-zero values mean a gate is running blind on its onSourceError policy.
	MetricSourceErrorsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "stageset_metric_source_errors_total",
		Help: "Metric-source query failures across the error-budget and promotion-analysis gates.",
	}, []string{"namespace", "name"})

	// StageBudgetFrozen is 1 while a stage's own errorBudget is holding a
	// new-revision entry into that stage (0 under dryRun, which records but does
	// not enforce). The per-stage analogue of BudgetFrozen.
	StageBudgetFrozen = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "stageset_stage_budget_frozen",
		Help: "Whether a stage's own error-budget freeze is holding a new-revision entry (1) or not (0).",
	}, []string{"namespace", "stageset", "stage"})

	// LedgerAnchorErrorsTotal counts scope: Lifetime completions whose
	// completionAnchor could not be read at gate time (RBAC gap, API error). Such
	// a completion is retained (fail open) rather than invalidated — a runaway
	// reading here would re-run a destructive bootstrap because of an outage — so
	// a non-zero rate is a signal to grant the stage's SA read on the anchor kind,
	// not an incident.
	LedgerAnchorErrorsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "stageset_ledger_anchor_errors_total",
		Help: "Total scope: Lifetime completions whose completionAnchor was unreadable at gate time (retained, fail open).",
	}, []string{"namespace", "name"})

	// LedgerAdoptionsTotal counts StageSets that, on their first reconcile, adopted
	// a StageLedger already carrying completions — a delete+recreate, or a fresh
	// StageSet over a retained ledger. A completion may suppress an action that
	// would otherwise run, so a surprise adoption is worth surfacing.
	LedgerAdoptionsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "stageset_ledger_adoptions_total",
		Help: "Total StageSets that adopted a non-empty StageLedger on their first reconcile.",
	}, []string{"namespace", "name"})
)

func init() {
	ctrlmetrics.Registry.MustRegister(ReconcileTotal, StageAppliedTotal, ActionRunsTotal, DriftCorrectedTotal, UpdateDeferredTotal, WebhookCertRenewalFailuresTotal, WatchEngagementFailuresTotal, TeardownForceDropTotal, InventorySkippedEntriesTotal, StageReady, StagePromotionPending, StagePromotionBlocked, BudgetRemaining, BudgetFrozen, MetricSourceErrorsTotal, StageBudgetFrozen, LedgerAnchorErrorsTotal, LedgerAdoptionsTotal)
}

// SetStageBudgetFrozen publishes the per-stage error-budget freeze gauge.
func SetStageBudgetFrozen(namespace, stageset, stage string, frozen bool) {
	StageBudgetFrozen.WithLabelValues(namespace, stageset, stage).Set(boolValue(frozen))
}

// SetStagePromotionPending publishes the promotion-gate gauge for one stage.
func SetStagePromotionPending(namespace, stageset, stage string, pending bool) {
	StagePromotionPending.WithLabelValues(namespace, stageset, stage).Set(boolValue(pending))
}

// SetStagePromotionBlocked publishes the analysis-failure gauge for one stage.
func SetStagePromotionBlocked(namespace, stageset, stage string, blocked bool) {
	StagePromotionBlocked.WithLabelValues(namespace, stageset, stage).Set(boolValue(blocked))
}

// SetBudgetFrozen publishes the error-budget freeze gauge for a StageSet.
func SetBudgetFrozen(namespace, name string, frozen bool) {
	BudgetFrozen.WithLabelValues(namespace, name).Set(boolValue(frozen))
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
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
	StagePromotionPending.DeletePartialMatch(prometheus.Labels{"namespace": namespace, "stageset": stageset})
	StagePromotionBlocked.DeletePartialMatch(prometheus.Labels{"namespace": namespace, "stageset": stageset})
	StageBudgetFrozen.DeletePartialMatch(prometheus.Labels{"namespace": namespace, "stageset": stageset})
}

// DeleteStageReadyForStage removes the readiness series for a single stage. It
// is called when a stage is dropped from a live StageSet's spec, so a
// metric-based rollout gate doesn't keep reading a phantom stage's last value.
func DeleteStageReadyForStage(namespace, stageset, stage string) {
	StageReady.DeleteLabelValues(namespace, stageset, stage)
	StagePromotionPending.DeleteLabelValues(namespace, stageset, stage)
	StagePromotionBlocked.DeleteLabelValues(namespace, stageset, stage)
	StageBudgetFrozen.DeleteLabelValues(namespace, stageset, stage)
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
	BudgetRemaining.DeletePartialMatch(nameMatch)
	BudgetFrozen.DeletePartialMatch(nameMatch)
	MetricSourceErrorsTotal.DeletePartialMatch(nameMatch)
}
