---
title: Error-budget freeze
description: Freeze new-revision rollouts while a service is out of its SLO error budget, resuming on its own when it recovers.
tags: [error-budget, slo, scheduling, metrics]
---

An error-budget freeze holds new-revision rollouts while a service is out of its
SLO error budget, and resumes them on its own once the budget recovers — the
Google SRE error-budget policy: when the budget is spent, stop shipping feature
changes until reliability is back. The controller does no SLO math; it reads one
number (the remaining budget) from a metric source you already run and compares
it to a threshold.

## Freeze on a Prometheus query

Point `spec.errorBudget` at an instant query that returns the remaining budget as
a scalar — every SLO tool exposes this (Sloth records
`slo:period_error_budget_remaining:ratio` directly; Pyrra, Grafana SLO, and
Nobl9's Prometheus API yield it in one line):

```yaml
apiVersion: stages.metio.wtf/v1
kind: StageSet
metadata:
  name: checkout
  namespace: apps
spec:
  serviceAccountName: deployer
  errorBudget:
    source:
      prometheus:
        address: http://prometheus.monitoring:9090
        query: slo:period_error_budget_remaining:ratio{sloth_service="checkout",sloth_slo="availability"}
    freezeThreshold: "0"      # freeze when remaining < this; "0" = only when overspent
    resumeThreshold: "0.05"   # resume only once it recovers to here (hysteresis)
    interval: 5m              # re-check cadence while frozen; defaults to spec.interval
  stages:
    - name: prod
      sourceRef:
        name: checkout-prod
```

While the remaining budget is below `freezeThreshold`, the StageSet reports
`Ready=False` with reason `BudgetExhausted` (an already-deployed StageSet stays
`Ready=True` and records the freeze on `status.budgetFreeze`). The current
revision keeps having its drift corrected — a frozen service still gets its
declared state enforced — and only new-revision rollouts wait. The controller
re-checks every `interval` and advances on its own once the budget reaches
`resumeThreshold`.

`freezeThreshold` is required and has no default. `resumeThreshold` defaults to
`freezeThreshold` (no hysteresis); set it higher to stop a budget hovering at the
line from flapping the freeze. `interval` defaults to the StageSet's reconcile
interval.

## It composes with update windows

`spec.errorBudget` and [`spec.updateWindows`](/usage/update-windows/) are combined
under a logical AND: a new revision rolls out only if the update window is open **and** the budget is
healthy. A closed window holds the rollout even when the budget is fine (and the
budget source isn't even queried), and an exhausted budget holds it even inside
an open window. Use both to deploy only during a maintenance window *and* only
while in budget.

## Authenticating the query

When Prometheus needs a bearer token, reference a Secret in the StageSet's
namespace with the token under the `token` key:

```yaml
  errorBudget:
    source:
      prometheus:
        address: https://prometheus.monitoring:9090
        query: slo:period_error_budget_remaining:ratio{sloth_service="checkout"}
        secretRef:
          name: prometheus-auth
```

The source address is dialed with the same SSRF guard as HTTP actions: loopback,
link-local, cloud-metadata, multicast, and unspecified addresses are refused,
while in-cluster private addresses (where Prometheus usually lives) are allowed.

## When the source is unreachable

`onSourceError` decides what happens when the query can't be read (Prometheus
down, a non-2xx, an empty result, `NaN`):

- `Allow` (default) — proceed. Blocking a rollout-wide freeze would stop *every*
  deploy, including the hotfix you need during the very outage that took the
  source down, so the freeze fails open. The error is still loud: a
  `BudgetSourceUnavailable` Warning event and the `stageset_metric_source_errors_total`
  metric. See the [BudgetSourceUnavailable runbook](/runbooks/budgetsourceunavailable/).
- `Hold` — block. Set this for a service that must stop deploying when its SLO
  source is down, accepting that a source outage then blocks its rollouts.

A misconfigured query that always returns `NaN` would silently fail open, so set
`dryRun: true` to prove a freeze rule fires before it gates — it records what
*would* freeze on `status.budgetFreeze` and the metrics, without holding
anything.

## Shipping a reliability fix while frozen

The error-budget policy explicitly exempts reliability and security fixes. Break
the glass to apply a held rollout once, without disabling the gate:

```shell
stagesetctl reconcile checkout --namespace apps --budget-override
```

## What it observes

The freeze reads *remaining budget*, not *burn rate* — a fast-burning but
not-yet-exhausted service still rolls out. Multi-window burn-rate alerting is the
SLO tool's job. To gate on observed behavior per stage (error rate, latency,
burn rate) rather than rollout-wide, use a
[promotion analysis](/usage/stage-promotion/), which shares this same metric
source contract.
