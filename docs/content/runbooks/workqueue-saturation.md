---
title: Workqueue saturation
description: The controller cannot drain its reconcile queue fast enough.
tags: [runbooks, metrics, alerts, troubleshooting]
---

## Symptom

`workqueue_depth{controller="stageset"}` stays high; StageSets reconcile slowly or
lag behind their `spec.interval`. The `StageSetControllerWorkqueueDepthHigh` alert
fires (see [operations](/installation/operations/) for the alert set and its
thresholds).

## Cause

The controller is enqueuing reconcile requests faster than it completes them.
Common causes:

- **apiserver slowness** — applies, dry-runs, and status writes all block on the
  apiserver (or the impersonated tenant's authorization).
- **slow sources** — a stage waiting on a large artifact fetch or a source that is
  slow to become Ready holds a worker.
- **a stuck stage** — an action with a long timeout (a `wait`/`http`/`job` that
  never completes) pins a worker for the whole timeout.
- **too few workers for the StageSet count** — many StageSets reconciling on short
  intervals.

## Diagnosis

```shell
# which StageSets are churning?
kubectl get stagesets -A --sort-by=.status.observedGeneration
# controller logs for slow operations / retries
kubectl -n stageset-system logs deploy/stageset-controller --tail=200
```

Correlate with `controller_runtime_reconcile_time_seconds` (see
[reconcile latency](/runbooks/reconcile-latency/)) and apiserver latency.

## Remediation

- Lengthen `spec.interval` on high-churn StageSets that don't need fast
  reconciliation.
- Cap long-running actions with a tighter `timeout` so a stuck action releases its
  worker.
- Adding replicas does **not** help: leader election means only the lease holder
  reconciles, so a second replica is failover, not added throughput
  ([production](/installation/production/#high-availability)). The controller has no
  reconcile-concurrency flag — the levers are reducing load (longer intervals, fewer
  StageSets, fewer objects per stage) and removing the slow operations below.
- Investigate apiserver / tenant-authorization latency if reconciles are uniformly
  slow.
