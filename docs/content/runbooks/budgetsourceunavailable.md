---
title: BudgetSourceUnavailable
description: A metric source for an error-budget freeze or a promotion analysis could not be read.
tags: [runbooks, error-budget, slo, metrics, troubleshooting]
---

## Symptom

A `BudgetSourceUnavailable` Warning event, the `stageset_metric_source_errors_total` counter rising, and — when a gate's `onSourceError` is `Hold` — `READY=False`, `REASON=BudgetSourceUnavailable`. With `onSourceError: Allow` the rollout proceeds and only the event and metric fire, so the condition may not show the reason.

## Cause

A metric source (the [error-budget freeze](/gating/error-budget/) or a [promotion analysis](/gating/stage-promotion/) check) was unreachable or returned no usable scalar: the Prometheus endpoint was down or returned a non-2xx, the query evaluated to an error, an empty or multi-sample vector, or `NaN` (the shape a wrong query takes). This is treated as *transient* — the controller keeps retrying.

What happens to the rollout depends on the gate's `onSourceError`:

- The **error-budget freeze** defaults to `Allow` (fail-open): blocking it would stop every deploy, including the hotfix you need during the very outage that took the source down.
- A **promotion analysis** defaults to `Hold` (fail-closed): holding only parks the rollout at the current healthy stage, so it is safe to refuse to advance while behavior can't be verified.

## Diagnosis

```shell
kubectl --namespace <namespace> describe stageset <name>   # the event names the source error
kubectl --namespace <namespace> get stageset <name> --output jsonpath='{.status.stages[*].promotionState.lastAnalysis}'
```

Check the address and query in `spec.errorBudget.source` / `spec.stages[].promotion.analysis.checks[].source`. From a pod in the namespace, confirm the Prometheus endpoint is reachable and the query returns a single scalar:

```shell
curl -s '<address>/api/v1/query?query=<query>'
```

A `NaN`/empty result means the query's labels match nothing — fix the query, not the gate. A connection failure points at NetworkPolicy or a wrong address. If a bearer token is configured, confirm the referenced Secret has a non-empty `token` key.

## Remediation

Fix the source so the query returns a single numeric scalar. While debugging a freeze, `spec.errorBudget.dryRun: true` records what would happen without holding anything. If a service is genuinely safety-critical and you want it to stop deploying when its SLO source is down, set `spec.errorBudget.onSourceError: Hold` (accepting that a source outage then blocks its deploys).
