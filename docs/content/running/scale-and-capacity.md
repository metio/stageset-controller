---
title: Scale and capacity
description: Size a controller replica for your workload, decide how many replicas to run, tune the throughput knobs, and read the metrics that tell you when you are at capacity.
tags: [scale, capacity, sizing, resources, leader-election, throughput, tuning]
---

Size the controller for the number and size of the `StageSet`s it manages, then
tune its cadence and inventory knobs to fit. Start from the chart's defaults,
raise resources only where a driver below applies to your release sizes, and watch
the saturation signals so you scale on evidence rather than guesswork.

## Sizing a replica

A reconcile fetches each stage's artifact, decrypts and builds it, applies the
rendered objects server-side, and polls readiness until the stage's gates pass.
CPU and memory scale with different parts of that pipeline.

### CPU

CPU is consumed by the build (kustomize render), the server-side apply of every
object, and the readiness polling that gates progression. A `StageSet` with many
stages, many objects per stage, or long-running readiness gates keeps a worker
busy longer. Because reconciles for distinct `StageSet`s are processed one at a
time (see [Tuning throughput](#tuning-throughput)), CPU pressure shows up as a
deeper workqueue and longer reconcile latency rather than as a pegged core. The
chart's default request of `50m` suits ordinary workloads; raise it when reconcile
latency climbs on a busy cluster.

### Memory

Resident memory is dominated by artifact fetching and the in-memory inventory. The
fetcher holds a stage's artifact in memory while it builds, and four independent
byte caps bound the worst case of a single fetch — they are internal constants,
not flags:

| Cap | Limit | Bounds |
| --- | --- | --- |
| `MaxArchiveBytes` | 64 MiB | the compressed download body |
| `MaxDecompressedBytes` | 512 MiB | the inflated gzip stream as it is read |
| `MaxPerEntryBytes` | 16 MiB | any single tar entry |
| `MaxExtractedBytes` | 64 MiB | the extracted result held in memory |

The extracted result a build sees is therefore capped at 64 MiB per stage, and the
gzip stream cannot inflate past 512 MiB even from a crafted archive — together
these defend against gzip bombs and cap the per-fetch footprint. The sharded
`StageInventory` adds memory proportional to how many objects each stage owns; very
large stages are split across shards (see
[`--inventory-shard-cap`](#tuning-throughput)), so no single inventory object grows
unbounded.

The chart's default request of `256Mi` covers ordinary inventories. Raise it for
clusters with very large stages (many thousands of applied objects) or large
artifacts. A defensible starting point for a busy cluster managing large releases:

```yaml
resources:
  cpu: 200m
  memory: 512Mi
  ephemeralStorage: 64Mi   # /tmp and the self-signed cert dir are emptyDirs
```

The chart sets requests equal to limits, so the pod is fully bounded; see
[Production](/running/production/#high-availability) for the resource block in
context.

## Replicas and HA

Reconciliation is single-leader. The chart enables leader election by default, so
even a single replica is lease-guarded, and only the lease holder reconciles —
adding replicas does **not** add reconcile parallelism. What extra replicas buy is
**failover** and **admission availability**: every replica answers the validating
webhook and the read-only gate endpoint, so a `kubectl apply` of a `StageSet` and a
Flagger gate check stay served during a leader handover.

The lease is not released eagerly on shutdown, so after a rolling update the new
leader takes over only when the old lease expires — budget a few seconds of
reconcile pause on restart. Admission and the gate endpoint stay available
throughout.

Running more than one replica adds one hard requirement: the optional
[rollback store](/gating/rollback/) must be reachable from whichever pod holds the
lease. A `ReadWriteOnce` PVC cannot satisfy that, so a multi-replica install needs
an **RWX `PersistentVolume`** or an **S3-compatible bucket**. The
[Production](/running/production/#high-availability) page has the full HA
setup, including the `HorizontalPodAutoscaler` and `PodDisruptionBudget` the chart
renders when the replica ceiling exceeds the floor; the leader model is detailed in
[multi-cluster and tenancy](/security/multi-cluster/).

## Tuning throughput

The knobs below control how often the controller reconciles and how it stores what
it applies. Each lists what it does, its default, when to change it, and the metric
that signals the need.

### Reconcile cadence — `--default-interval` and `spec.interval`

`--default-interval` (default `10m`) is the reconcile cadence a `StageSet` inherits
when it omits `spec.interval`. A source change reconciles immediately through the
artifact watch regardless of the interval; the interval is the periodic re-assert
that heals out-of-band drift. Lowering it makes the controller re-check applied
state more often — more responsive drift correction at the cost of more apiserver
and apply traffic per `StageSet`. A `StageSet` can override the global default per
release:

```yaml
apiVersion: stages.metio.wtf/v1
kind: StageSet
metadata:
  name: payments
  namespace: payments
spec:
  interval: 5m                  # re-assert applied state every 5 minutes
  retryInterval: 1m             # back off faster after a failed run
  driftDetectionInterval: 30m   # cheap drift sweep between full reconciles
  stages:
    - name: api
      sourceRef:
        apiVersion: source.toolkit.fluxcd.io/v1
        kind: ExternalArtifact
        name: payments-api
```

`spec.driftDetectionInterval`, when set, re-asserts applied state on its own cadence
between full reconciles, so you can keep the full `interval` long while still
healing drift cheaply. Watch `stageset_drift_corrected_total` to judge whether
drift is frequent enough to warrant a shorter sweep.

### Inventory shard size — `--inventory-shard-cap`

`--inventory-shard-cap` (default `5000`) is the maximum number of entries a single
`StageInventory` object holds. When a stage owns more objects than the cap, its
inventory is split across multiple shards so no one object grows unbounded.
Sharding is automatic — the cap is a tuning knob you rarely touch. Lower it if your
apiserver struggles with large `StageInventory` objects on very large applies;
raise it to keep a moderately large stage in a single object. The per-stage shard
count surfaces on the `StageSet` status, and the mechanism is described in
[Inventory and pruning](/defining-a-release/inventory/).

```yaml
# Helm values — controller-wide tuning.
controller:
  defaultInterval: 10m     # cadence StageSets inherit when they omit spec.interval
  inventoryShardCap: 5000  # lower for smaller inventory objects on huge applies
```

### Watch scope and reference isolation

`--watch-namespaces` (empty by default, meaning cluster-wide) scopes the manager's
informers to a namespace set, so one deployment per tenant group observes only its
own `StageSet`s — the multi-tenant controller-instances pattern. A scoped instance
caches fewer objects and reconciles fewer `StageSet`s, which is both an isolation
and a capacity lever. `--no-cross-namespace-refs` hard-denies cross-namespace
`sourceRef` and `dependsOn` references. Both are covered in
[multi-cluster and tenancy](/security/multi-cluster/).

## Knowing when you're at capacity

Scale on the saturation signals the controller exports, not on intuition. The
[metrics](/observability/metrics/) page lists every series; the ones that mean "at
capacity" are:

- **`workqueue_depth{controller="stageset"}`** — a depth that stays high means
  reconciles are arriving faster than the single-leader worker drains them. The
  `StageSetControllerWorkqueueDepthHigh` alert fires past a sustained depth; its
  runbook is [workqueue saturation](/runbooks/workqueue-saturation/).
- **`controller_runtime_reconcile_time_seconds`** — rising p99 reconcile latency
  points at CPU pressure or slow readiness gates. The
  `StageSetReconcileLatencyHigh` alert covers it; see the
  [reconcile latency runbook](/runbooks/reconcile-latency/).
- **`stageset_reconcile_total{reason=…}`** — a climbing non-`Succeeded` rate is a
  correctness or RBAC problem, not a sizing one; the
  `StageSetReconcileErrorsHigh` alert tracks it.
- **`stageset_drift_corrected_total`** and **`stageset_update_deferred_total`** —
  frequent drift correction argues for a `driftDetectionInterval`; frequent
  deferrals mean update windows are throttling delivery.

The chart ships these as a `PrometheusRule` with tunable thresholds — see
[Alerting](/observability/alerting/) — and the published
[dashboard](/observability/dashboard/) and [SLOs](/observability/slos/) render the
same signals. When the workqueue stays deep and latency is high while reconciles
still succeed, the lever is a larger CPU/memory request on the replica, a longer
`--default-interval`, or splitting the workload across watch-scoped instances —
adding replicas alone will not help, because only the leader reconciles.

## Related

- [Production](/running/production/#high-availability) — the hardened install, HA replicas, and the resource block in context.
- [Inventory and pruning](/defining-a-release/inventory/) — sharding and the `StageInventory` the shard cap governs.
- [Multi-cluster and tenancy](/security/multi-cluster/) — watch-scoped instances, the leader model, and cross-namespace isolation.
- [Metrics](/observability/metrics/) — every exported series and how to scrape them.
- [Alerting](/observability/alerting/) — the shipped saturation alerts and their thresholds.
- [Backup and disaster recovery](/running/disaster-recovery/) — the state a multi-replica install must keep reachable.
