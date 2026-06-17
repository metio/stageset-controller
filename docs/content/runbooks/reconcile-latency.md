---
title: Reconcile latency high
description: Reconcile p99 latency for the StageSet controller is above threshold.
tags: [runbooks, metrics, alerts, troubleshooting]
---

## Symptom

`controller_runtime_reconcile_time_seconds` p99 for `controller="stageset"` exceeds
the configured threshold; the `StageSetReconcileLatencyHigh` alert fires (see
[operations](/installation/operations/) for the alert set and its thresholds).

## Cause

A single reconcile does a lot of work — resolve and fetch every stage's artifact,
kustomize-build, server-side apply, prune, verify readiness, and run actions — all
impersonating the tenant `ServiceAccount`. Latency climbs when any of those is slow:

- large artifacts or slow artifact servers,
- many objects per stage (apply + prune scale with object count),
- readiness waits and `wait`/`http`/`job` actions with long timeouts,
- apiserver or tenant-authorization slowness.

## Diagnosis

```shell
kubectl --namespace stageset-system logs deploy/stageset-controller --tail=200 | grep -i 'slow\|timeout\|took'
```

Break the latency down by stage count and artifact size; a single StageSet with
many large stages dominates p99.

## Remediation

- Split a very large StageSet into smaller ones, or fewer objects per stage.
- Tighten action `timeout`s so a slow gate fails fast instead of stretching the
  reconcile.
- Raise `spec.interval` where freshness isn't critical.
- Address upstream artifact-server or apiserver latency.

If the queue itself is backing up, see [workqueue saturation](/runbooks/workqueue-saturation/).
