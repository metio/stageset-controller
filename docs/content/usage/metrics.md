---
title: Metrics
description: The controller's Prometheus metrics — the stageset_* custom series and controller-runtime signals, scraping with a ServiceMonitor or a plain config, querying with PromQL, and the chart values that drive them.
tags: [observability, metrics, prometheus, promql]
---

The controller exports Prometheus metrics on an HTTP endpoint: a set of custom
`stageset_*` series describing reconcile outcomes, stage applies, drift
correction, deferred rollouts, webhook-cert renewals, and per-stage readiness —
alongside the controller-runtime and workqueue series every controller-runtime
manager exposes for free.

## The controller binary

The controller binds its metrics endpoint via `--metrics-bind-address`, defaulting
to `:8080`. Setting it to `0` disables the endpoint. Every custom metric is
registered against controller-runtime's registry, so it rides the same endpoint as
the built-in series.

All counters and the readiness gauge are labelled by `namespace` and the StageSet
`name` so a single StageSet's behaviour is isolatable.

### Reconcile outcomes

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `stageset_reconcile_total` | Counter | `namespace`, `name`, `reason` | Reconciles by their terminal Ready-condition reason. The `reason` label is one of the wire-stable Ready reasons. |
| `stageset_update_deferred_total` | Counter | `namespace`, `name` | Reconciles that held a rollout because an update window was closed. |

### Stage progress

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `stageset_stage_applied_total` | Counter | `namespace`, `name`, `stage` | Stages that applied and verified successfully. |
| `stageset_drift_corrected_total` | Counter | `namespace`, `name`, `stage` | Objects whose out-of-band drift the apply corrected on a steady-state reconcile (same revision as last applied). |
| `stageset_stage_ready` | Gauge | `namespace`, `stageset`, `stage` | Whether a stage is Ready (`1`) or not (`0`) at the current observed state. A metrics-gated progressive-delivery controller — Argo Rollouts' Prometheus metric provider, for example — can hold a rollout directly on this gauge. The series is deleted when a StageSet or one of its stages goes away, so a removed stage leaves no stale gauge. |

### Webhook

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `stageset_webhook_cert_renewal_failures_total` | Counter | — | Failed self-signed webhook certificate renewals in the background renewer goroutine. Meaningful only in self-signed webhook mode. |

### Controller-runtime and workqueue metrics

The manager also exports the standard controller-runtime series without any extra
configuration, all carrying `controller="stageset"`:

- `controller_runtime_reconcile_total`, `controller_runtime_reconcile_errors_total`,
  and the `controller_runtime_reconcile_time_seconds` histogram.
- `workqueue_depth`, `workqueue_adds_total`, the `workqueue_queue_duration_seconds`
  and `workqueue_work_duration_seconds` histograms, and the retry counters.

### Querying with PromQL

A few starting points once the series are scraped:

```promql
# Reconciles settling to a non-happy reason, per StageSet, over 5m.
sum by (namespace, name, reason) (
  rate(stageset_reconcile_total{reason!~"Succeeded|Suspended"}[5m])
)

# Stages currently not Ready.
stageset_stage_ready == 0

# Reconcile p99 latency for the controller.
histogram_quantile(0.99,
  sum by (le) (rate(controller_runtime_reconcile_time_seconds_bucket{controller="stageset"}[10m]))
)
```

The [alerting](/usage/alerting/) catalog builds on exactly these expressions.

## The Helm chart

The chart binds the metrics port through `ports.metrics` (default `8080`) and
always renders a ClusterIP `Service` named `<release>-metrics` exposing a `metrics`
port that targets the container's `metrics` port, so any scrape mechanism has a
stable target.

### Scrape with a ServiceMonitor

When the Prometheus operator is installed, opt into a `ServiceMonitor` that
selects the metrics Service:

```yaml
metrics:
  serviceMonitor:
    enabled: true
    interval: 30s
    # Extra labels so your Prometheus instance's serviceMonitorSelector picks it up.
    additionalLabels:
      release: kube-prometheus-stack
```

### Scrape without the Prometheus operator

Without the operator's CRDs, point a plain Prometheus scrape config at the metrics
Service, or annotate the rendered Service for an annotation-driven scrape discovery
setup:

```shell
kubectl annotate service <release>-metrics \
  prometheus.io/scrape=true \
  prometheus.io/port=8080
```

For the full flag list with defaults, see the
[configuration reference](/installation/configuration/).
