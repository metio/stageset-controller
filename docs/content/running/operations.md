---
title: Operations
description: Metrics, alerts, events, and runbooks for running the controller day to day.
tags: [operations, metrics, alerts, runbooks]
---

## Metrics

The controller registers custom metrics on the controller-runtime registry, served
on `--metrics-bind-address` (`:8080`) alongside the standard
`controller_runtime_*` and `workqueue_*` series. Enable scraping with the chart's
opt-in `ServiceMonitor` (`metrics.serviceMonitor.enabled`):

```yaml
# values.yaml
metrics:
  serviceMonitor:
    enabled: true        # needs the Prometheus operator CRDs
```

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `stageset_reconcile_total` | counter | `namespace`, `name`, `reason` | Reconciles, by terminal Ready reason. |
| `stageset_stage_applied_total` | counter | `namespace`, `name`, `stage` | Stages applied and verified. |
| `stageset_drift_corrected_total` | counter | `namespace`, `name`, `stage` | Out-of-band drift re-asserted on a steady-state reconcile. |
| `stageset_update_deferred_total` | counter | `namespace`, `name` | Rollouts held by a closed update window. |
| `stageset_webhook_cert_renewal_failures_total` | counter | _(none)_ | Failed self-signed webhook cert renewals. |
| `stageset_teardown_force_drop_total` | counter | `namespace`, `name` | Finalizers force-dropped after `--max-teardown-wait` of failing teardown; sustained non-zero values flag an unreachable target and orphaned objects. See [TeardownForced](/runbooks/teardown-forced/). |
| `stageset_stage_ready` | gauge | `namespace`, `stageset`, `stage` | `1` when a stage is Ready, else `0` — for metric-based [progressive delivery](/guides/progressive-delivery/#argo-rollouts). |

## Alerts

The chart ships an opt-in `PrometheusRule` with a starter alert set, gated on
`metrics.prometheusRule.enabled` (requires the
[Prometheus operator](https://prometheus-operator.dev/) CRDs). It covers the
custom `stageset_*` metrics plus controller-runtime signals:

| Alert | Fires on | Severity |
|---|---|---|
| `StageSetReconcileErrorsHigh` | per-StageSet Ready=False rate (excludes the healthy `Succeeded`/`Suspended` reasons) | warning |
| `StageSetControllerWorkqueueDepthHigh` | the reconcile queue not draining | warning |
| `StageSetReconcileLatencyHigh` | reconcile p99 latency over threshold | warning |
| `StageSetControllerPodDown` | a controller pod NotReady | critical |
| `StageSetWebhookCertRenewalFailing` | self-signed cert rotation failing | critical |

Every threshold is a knob under `metrics.prometheusRule.thresholds`, and
`extraAlertLabels` is merged onto every rendered alert so all stageset alerts can
route through one Alertmanager receiver. Each alert carries a `runbook_url`
annotation pointing at the matching [runbook](/runbooks/) page on this site; the
URL prefix is fixed to this site, and the reconcile-errors alert templates the
URL on `$labels.reason`. Append your own rules under
`metrics.prometheusRule.extraRules`, and silence a built-in alert by raising its
threshold rather than forking the chart. When `StageSetControllerWorkqueueDepthHigh`
or `StageSetReconcileLatencyHigh` fires persistently, the controller is at capacity
— see [Scale and capacity](/running/scale-and-capacity/) for which lever to
pull.

## Events

The controller emits Kubernetes Events on every Ready-condition transition, so
`kubectl describe stageset <name>` and [Flux](https://fluxcd.io/)'s
`notification-controller` (via an `Alert` targeting `kind: StageSet`) both
surface what happened. Normal events
include `Succeeded`, `UpdateDeferred`, `MigrationStarted`, and
`MigrationCompleted`; warnings include `StageFailed`, `DriftCorrected`,
`RolledBack`, `MigrationFailed`, `OnFailureAction`, `RollbackStoreFailed`, and
`TeardownForced` (a deleting StageSet's finalizer force-dropped after
`--max-teardown-wait` — see [TeardownForced](/runbooks/teardown-forced/)).

## Runbooks

Every actionable Ready-condition reason has a [runbook](/runbooks/) covering the
symptom, cause, diagnosis, and remediation. The controller appends a
`(runbook: https://stageset.projects.metio.wtf/runbooks/<reason>/)` link to every
actionable Ready message (the reason lower-cased into a path segment), so a
`kubectl describe` links straight to the fix. Healthy reasons (`Succeeded`,
`Suspended`) get no link.

For example, a `StageFailed` StageSet shows:

```text
Message:  stage "application" failed: … (runbook: https://stageset.projects.metio.wtf/runbooks/stagefailed/)
```

## Forcing a reconcile

The controller reconciles on its `spec.interval`, on source changes, and on
demand. To trigger an out-of-band run, stamp the standard annotation — which is
what `flux reconcile` and [`stagesetctl reconcile`](/cli/reconcile/) do for you:

```shell
kubectl annotate stageset my-app \
  reconcile.fluxcd.io/requestedAt="$(date -u +%FT%TZ)" --overwrite
```

The handled token is recorded in `status.lastHandledReconcileAt`.

## Drift correction

On a steady-state reconcile the controller re-asserts the desired state, healing
out-of-band changes to managed objects. Each correction emits a `DriftCorrected`
event and increments `stageset_drift_corrected_total`. Tighten the cadence with
`spec.driftDetectionInterval` when you need faster healing than `spec.interval`.
