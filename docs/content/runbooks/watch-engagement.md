---
title: Producer watch engagement failing
description: A dynamic watch on a producer source kind failed to engage, so StageSets referencing that kind no longer re-trigger on the producer's upstream changes.
tags: [runbooks, troubleshooting, sources]
---

Fires when `stageset_watch_engagement_failures_total{gvk=...}` has increased
above the per-hour threshold for the alert window. The controller engages
producer watches lazily: the first time a StageSet names a producer kind in a
stage's `sourceRef`, `engageProducerWatch` adds a dynamic `source.Kind` watch
(routed through `mapProducer`) so a change on that producer re-triggers the
referencing StageSets. `ExternalArtifact` is watched statically; the dynamic
watches cover producer kinds such as `GitRepository`, `OCIRepository`, `Bucket`,
and operator-defined producers like a JaaS `JsonnetSnippet`.

When that engagement fails, the producer kind stays unwatched. The visible
symptom is that StageSets referencing that kind stop re-triggering on the
producer's upstream updates — they sit at their last-resolved revision and only
catch up on the periodic retry. There is no per-StageSet status signal.

## Symptom

- `StageSetWatchEngagementFailing` alert is firing with `gvk` labelling the
  affected producer kind.
- StageSets whose stages reference that kind don't react to upstream producer
  changes promptly — they reconcile only on `retryInterval`, not on the
  producer's status flip.
- The producer's source CRs (`GitRepository`, `OCIRepository`, `Bucket`,
  `ExternalArtifact`) show recent `status.artifact.revision` changes that the
  StageSets aren't picking up between periodic reconciles.

## Cause

A watch engagement can fail for two reasons:

- **Transient cache reconnect.** controller-runtime's shared cache can drop and
  re-establish its informer connection under load; a `Controller.Watch` call
  during that window can fail once. The controller does not record the GVK as
  watched on failure, so a later reconcile re-attempts and the watch engages on
  its own. A single blip at startup or during a cache reconnect is expected.
- **Missing ClusterRole verb on the producer kind.** If the controller's
  ClusterRole lacks `get`/`list`/`watch` on the producer kind, every engagement
  attempt fails and the watch never engages — a stuck watch that the periodic
  reconcile cannot heal.

## Diagnosis

### Step 1 — confirm the producer CRD is installed and Established

```shell
kubectl get crd <plural>.<group> \
  --output jsonpath='{.status.conditions[?(@.type=="Established")].status}{"\n"}'
```

Expect `True`. If the CRD is not installed the controller is correct to skip the
watch; install the producer's controller (Flux source-controller, JaaS, …).

### Step 2 — check the controller's RBAC on the producer kind

```shell
kubectl auth can-i watch <plural>.<group> \
  --as=system:serviceaccount:<ns>:<controller-sa>
kubectl auth can-i list <plural>.<group> \
  --as=system:serviceaccount:<ns>:<controller-sa>
```

If either is "no", the chart's controller ClusterRole is missing the
`get/list/watch` verbs on this kind. Grant them.

### Step 3 — inspect controller-runtime cache state

```shell
kubectl --namespace <ns> logs deploy/stageset-controller \
  | grep -E 'watch|cache|forbidden' | tail -20
```

Look for `cache reconnect`, `informer failed`, or `Watch failed: forbidden`. A
transient reconnect trips engagement once and heals on the next reconcile;
sustained `forbidden` points at RBAC.

## Remediation

1. **Fix the RBAC / verb** issue identified above so the controller can watch
   the producer kind.
2. **Roll the controller pod** to force a fresh setup pass and re-engage the
   watch on the next reconcile that touches a referencing StageSet:

   ```shell
   kubectl --namespace <ns> rollout restart deployment stageset-controller
   ```

3. **Verify** the counter stops increasing and the alert clears.

## When the alert is noisy

If `stageset_watch_engagement_failures_total` ticks once during startup or a
cache reconnect but never again, that is the expected self-healing behavior: the
GVK was not recorded as watched, so the next reconcile re-engaged it. Raise
`metrics.prometheusRule.thresholds.watchEngagementFailuresPerHour` if the blip is
noisy enough to page.
