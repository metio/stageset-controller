---
title: Alerting
description: The controller's Kubernetes Events on StageSet transitions, the opt-in PrometheusRule alert catalog with tunable thresholds, and how every alert and Ready reason links to a runbook.
tags: [observability, alerts, prometheus, events, runbooks]
---

Two layers surface trouble: Kubernetes Events the controller emits on a StageSet's
own timeline, and a Prometheus alert set built on the [metrics](/usage/metrics/).
Both lead an operator to a [runbook](/runbooks/).

## The controller binary

### Kubernetes Events

On a StageSet's condition transitions and notable actions, the controller emits
standard Kubernetes Events (events.v1) against the StageSet object. The reason
fills both the Event `reason` and `action` slots. Read them with:

```shell
kubectl --namespace team-a describe stageset checkout
# or, just the events:
kubectl --namespace team-a get events --field-selector involvedObject.name=checkout
```

Reasons you will see include `Ready` and `StageFailed` on the rollout outcome,
`UpdateDeferred` when an update window holds a rollout, `MigrationStarted` /
`MigrationCompleted` / `MigrationFailed` around a versioned migration,
`DriftCorrected` when an apply repaired out-of-band drift, `RolledBack` when
`rollbackOnFailure` restored the last-good revisions, and `RollbackStoreFailed` /
`OnFailureAction` for best-effort side paths. Because Events target a `StageSet`,
Flux's `notification-controller` can route them to Slack, Teams, or any provider by
declaring an `Alert` that targets `kind: StageSet` — no extra plumbing on the
controller side.

### Ready reasons link to runbooks

Every actionable Ready-condition reason maps to a runbook page at
`/runbooks/<reason>/`. The controller threads a
`(runbook: https://stageset.projects.metio.wtf/runbooks/<reason>/)` suffix onto
actionable Ready-condition messages, so `kubectl describe stageset` links straight
to the remediation page. Intentional, steady states — `Succeeded`, `Suspended` —
carry no suffix, since there is nothing to remediate.

## The Helm chart

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

### Alerts link to runbooks

Each alert carries a runbook link in its `runbook_url` annotation (the annotation
key is `metrics.prometheusRule.runbookAnnotationKey`; the URL prefix is fixed to
this site's [runbooks](/runbooks/)). `StageSetReconcileErrorsHigh` templates its
link on the `reason` label — every Ready-condition reason maps to a runbook at
`/runbooks/<reason>/` — while the availability and webhook alerts point at their
fixed pages ([workqueue-saturation](/runbooks/workqueue-saturation/),
[reconcile-latency](/runbooks/reconcile-latency/),
[controller-pod-down](/runbooks/controller-pod-down/),
[webhook-cert-renewal](/runbooks/webhook-cert-renewal/)).
