---
title: Observability
description: The controller's Prometheus metrics, the chart's metrics Service and ServiceMonitor, the opt-in PrometheusRule alert set, and how every alert links to a runbook.
tags: [observability, metrics, prometheus, alerts]
---

The controller exports Prometheus metrics on an HTTP endpoint: a set of custom
`stageset_*` series describing reconcile outcomes, stage applies, drift
correction, deferred rollouts, webhook-cert renewals, and per-stage readiness —
alongside the controller-runtime and workqueue series every controller-runtime
manager exposes for free. The Helm chart wires a metrics `Service`, an opt-in
`ServiceMonitor` for scraping, and an opt-in `PrometheusRule` with a starter
alert set whose thresholds are tunable values.

## The metrics endpoint

The controller binds its metrics endpoint via `--metrics-bind-address`, defaulting
to `:8080`. Setting it to `0` disables the endpoint.

The chart binds the same port through `ports.metrics` (default `8080`) and renders
a ClusterIP `Service` named `<release>-metrics` exposing a `metrics` port that
targets the container's `metrics` port. The Service is always present, so any
scrape mechanism has a stable target.

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

## Metrics reference

Every custom metric is registered against controller-runtime's registry, so it
rides the same `--metrics-bind-address` endpoint as the built-in series. All
counters and the readiness gauge are labelled by `namespace` and the StageSet
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

The shipped alerts build on both the custom `stageset_*` series and these
controller-runtime signals.

## Alerts

The chart ships an opt-in `PrometheusRule` named `<release>-alerts` with a starter
alert set. It requires the Prometheus operator CRDs. Enable it and route every
alert to one Alertmanager receiver with `extraAlertLabels`:

```yaml
metrics:
  prometheusRule:
    enabled: true
    interval: 30s
    # Labels Prometheus selects the rule object on.
    labels:
      release: kube-prometheus-stack
    # Merged onto every rendered alert, so all stageset alerts route together.
    extraAlertLabels:
      team: platform
```

### Shipped alerts

| Alert | Severity | Fires when | Threshold knobs |
| --- | --- | --- | --- |
| `StageSetReconcileErrorsHigh` | warning | Per-StageSet Ready=False rate (5m window) exceeds the rate, excluding the happy `Succeeded` and `Suspended` reasons. | `reconcileErrorRate` (`0.1`/s), `reconcileErrorDuration` (`10m`) |
| `StageSetControllerWorkqueueDepthHigh` | warning | `workqueue_depth{controller="stageset"}` exceeds the depth. | `workqueueDepth` (`50`), `workqueueDuration` (`15m`) |
| `StageSetReconcileLatencyHigh` | warning | Reconcile p99 (10m window) exceeds the ceiling in seconds. | `reconcileLatencySeconds` (`30`), `reconcileLatencyDuration` (`15m`) |
| `StageSetControllerPodDown` | critical | A controller pod is NotReady for the window. | `podDownDuration` (`5m`) |
| `StageSetWebhookCertRenewalFailing` | critical | `stageset_webhook_cert_renewal_failures_total` increases over 1h beyond the count. | `webhookCertRenewalFailuresPerHour` (`1`), `webhookCertRenewalFailuresDuration` (`30m`) |

Each threshold lives under `metrics.prometheusRule.thresholds`. To silence a
built-in alert without forking the chart, raise its threshold to an impossibly high
value; to add alerts, append them under `metrics.prometheusRule.extraRules`, which
renders verbatim in a separate `stageset-extras` group.

```yaml
metrics:
  prometheusRule:
    thresholds:
      reconcileErrorRate: 0.1
      reconcileErrorDuration: 10m
      workqueueDepth: 50
      workqueueDuration: 15m
      reconcileLatencySeconds: 30
      reconcileLatencyDuration: 15m
      podDownDuration: 5m
      webhookCertRenewalFailuresPerHour: 1
      webhookCertRenewalFailuresDuration: 30m
```

## Runbooks

Each alert carries a runbook link in its `runbook_url` annotation (the annotation
key is `metrics.prometheusRule.runbookAnnotationKey`, the URL prefix is
`metrics.prometheusRule.runbookBaseURL`, defaulting to the documentation site's
[runbooks](/runbooks/)). `StageSetReconcileErrorsHigh` templates
its link on the `reason` label — every Ready-condition reason maps to a runbook at
`/runbooks/<reason>/` — while the availability and webhook alerts point at their
fixed pages (`workqueue-saturation`, `reconcile-latency`, `controller-pod-down`,
`webhook-cert-renewal`).

The same reason-to-page mapping surfaces on the resource itself: the controller's
`--runbook-base-url` flag threads a `(runbook: <base>/<reason>/)` suffix onto
actionable Ready-condition messages, so `kubectl describe stageset` links straight
to the remediation page for an actionable failure. Intentional and steady states —
`Succeeded`, `Suspended` — carry no suffix, since there is nothing to remediate.
