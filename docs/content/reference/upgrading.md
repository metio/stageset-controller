---
title: Upgrading
description: The actions required when upgrading stageset-controller, one section per release that needs migration steps.
tags: [installation, upgrade, migration, releases]
---

This page lists the actions required when upgrading stageset-controller. Each
release that needs action gets a section below, headed by its calendar version; a
release with no section needs no migration — a plain upgrade (bump the chart's
`appVersion` or pull the new image tag) suffices.

## 2026.7.31184504

A stage that is still coming up no longer reports as a failure, and the
controller reconciles several StageSets at once. Only the first item below needs
anything from you, and only if you alert or gate on `StageFailed`.

### A slow stage now reports `StageProgressing`

A stage whose verify wait ran out of time while its objects were still making
progress used to report `Ready=False / StageFailed`. It now reports
[`StageProgressing`](/runbooks/stageprogressing/) — its own reason, because a
release that is still starting and one that broke need different answers. The
first converges on its own; only the second wants a human.

A stage that genuinely failed is unaffected: the verify wait still aborts the
moment an object reaches a terminal state, and still reports `StageFailed`.

Anything matching on `StageFailed` therefore sees less than it used to. That is
usually the point — a slow first install stops paging — but a rule that was
counting on it to catch *everything* now has a gap. Check both places the reason
surfaces:

```shell
# Alerting and dashboards: the reason is a label on the reconcile counter.
grep -rn 'StageFailed' <your-prometheus-rules-and-dashboards>

# Anything polling the Ready condition (a portal, a fleet gate, a CI wait).
kubectl get stageset --all-namespaces \
  -o custom-columns='NS:.metadata.namespace,NAME:.metadata.name,REASON:.status.conditions[?(@.type=="Ready")].reason'
```

Add `StageProgressing` where you want "still coming up" treated as an alert, or —
more usually — leave it out and let it stop waking people.

The hold is also paced by `spec.retryInterval` (falling back to `spec.interval`)
rather than by the controller's internal backoff, so the re-check cadence is now
a number you set.

### Reconciles run four at a time

A stage's verify wait blocks its worker for the whole stage timeout, and the
controller previously ran one reconcile at a time — so one tenant coming up
slowly held every other tenant's reconcile behind it, for as long as it took.
Four run concurrently now.

Nothing to configure. Memory scales with it: four reconciles can each hold a
fetched artifact and its rendered manifests, so a controller sized against the
old serial behaviour on very large releases may want its limit raised.

## 2026.7.31092513

How long a stage may take to verify, and what happens when it runs out of time,
both changed. Only the first item below needs an audit before upgrading.

### `readyChecks.timeout` now takes effect

`stages[].readyChecks.timeout` was accepted and then ignored: a stage that
declared how long its readiness takes was bounded by `spec.timeout` instead. It
is now the most specific level of the timeout ladder —
`readyChecks.timeout`, then `stages[].timeout`, then `spec.timeout`, then the
default.

This is the one change that can make a timeout **shorter** than before. A
StageSet written from the previous documentation carries `readyChecks.timeout:
5m`, which was doing nothing and now caps the verify phase at five minutes,
below the new fifteen-minute default. List every stage that carries one:

```shell
kubectl get stageset --all-namespaces --output=json \
  | jq -r '.items[] | . as $s | .spec.stages[]?
           | select(.readyChecks.timeout != null)
           | "\($s.metadata.namespace)/\($s.metadata.name) stage \(.name): \(.readyChecks.timeout)"'
```

Raise or remove the value on any stage whose workload needs longer than it says.

### The default stage timeout is 15 minutes

A stage with no timeout at any level now waits 15 minutes instead of 5 before
failing. Nothing to do: the verify wait already fails fast when an object reaches
a terminal failure, so the extra patience applies only to workloads still making
progress — a first install running migrations against an empty database is the
usual one.

### A verify timeout no longer rolls back

With `spec.rollbackOnFailure` set, a stage that ran out of time used to have its
previous manifests restored. It now halts with the new manifests in place, and
the next reconcile re-verifies them. A stage whose objects genuinely failed still
rolls back.

Set `onTimeout: Rollback` — on `spec`, or on a single stage — to keep the old
behaviour. Doing so is worth a thought for anything that migrates a database
before it serves: restoring older code over a half-finished migration is what the
new default avoids.
