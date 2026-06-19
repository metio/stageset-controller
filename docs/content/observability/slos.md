---
title: Service level objectives
description: The StageSet controller's SLOs — reconcile availability and reconcile latency — with their SLIs, targets, error budget, the dashboard that shows them, and how to tune them.
tags: [controller, slo, metrics, alerts]
---

The StageSet controller tracks two service-level objectives. Each is an objective
on a service-level indicator (SLI) computed from the
[metrics](/observability/metrics/), measured over a rolling window. The published
[dashboard](/observability/dashboard/) renders them, and the Helm chart can alert
on them.

## SLO 1 — reconcile availability

**Objective:** ≥ **99%** of syncing reconciles reach `Ready=True` (reason
`Succeeded`), over a **28-day** window.

The SLI counts only reconciles that were actually trying to sync. The intentional
and upstream-waiting reasons are excluded from the failure half — a paused
StageSet (`Suspended`), a rollout held by an update window (`UpdateDeferred`), and
a wait on an upstream artifact or dependency (`SourceNotReady`,
`DependencyNotReady`) are not failures and must not burn the budget:

```promql
sum(rate(stageset_reconcile_total{reason="Succeeded"}[28d]))
/
(
  sum(rate(stageset_reconcile_total{reason="Succeeded"}[28d]))
  + sum(rate(stageset_reconcile_total{reason!~"Succeeded|Suspended|UpdateDeferred|SourceNotReady|DependencyNotReady"}[28d]))
)
```

The **error budget** is the 1% of reconciles allowed to fail over the window.
Remaining budget, normalised so `1` is full and `0` is exhausted:

```promql
(<availability> - 0.99) / (1 - 0.99)
```

## SLO 2 — reconcile latency

**Objective:** the StageSet controller's **p95** reconcile duration stays
**below 30s** over the window.

```promql
histogram_quantile(0.95, sum by (le) (
  rate(controller_runtime_reconcile_time_seconds_bucket{controller="stageset"}[28d])
))
```

## See the SLOs on the dashboard

The published [dashboard](/observability/dashboard/) opens with an SLO band:
current availability against its objective, error budget remaining, p95 latency
against its objective, and an availability-versus-objective trend. The
controller-internals panels below explain any movement.

The objectives and window are top-level arguments, so you set them per environment
when you render the dashboard through a JaaS `JsonnetSnippet`:

```yaml
spec:
  tlas:
    datasource: ["prometheus"]   # your Prometheus datasource UID
    window: ["28d"]              # SLO window
    availabilityTarget: ["0.99"] # 99%
    latencyTarget: ["30"]        # seconds
```

A short window is fine for a demo; a real `28d` SLI needs at least that much
Prometheus retention. For long windows, precompute the SLI with a recording rule
and point `window` at the recorded series instead of a raw `rate(...[28d])`.

## Alert on the budget

The shipped [alerts](/observability/alerting/) already page on the *causes* of SLO
loss (reconcile errors, latency, workqueue saturation). To alert on the objective
itself, add an availability-SLO rule through the chart's `extraRules` passthrough —
here it fires when recent availability drops below 99%:

```yaml
metrics:
  prometheusRule:
    enabled: true
    extraRules:
      - alert: StageSetReconcileAvailabilityBelowSLO
        expr: |
          (
            sum(rate(stageset_reconcile_total{reason="Succeeded"}[1h]))
            /
            (
              sum(rate(stageset_reconcile_total{reason="Succeeded"}[1h]))
              + sum(rate(stageset_reconcile_total{reason!~"Succeeded|Suspended|UpdateDeferred|SourceNotReady|DependencyNotReady"}[1h]))
            )
          ) < 0.99
        for: 1h
        labels:
          severity: warning
        annotations:
          summary: StageSet reconcile availability is below its 99% objective
```

The alert measures a short recent window (`1h`) so it pages while the budget is
actively burning; the dashboard's `window` shows the full SLO window. See
[Alerting](/observability/alerting/) for the rest of the catalog.
