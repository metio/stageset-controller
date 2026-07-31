---
title: Upgrading
description: The actions required when upgrading stageset-controller, one section per release that needs migration steps.
tags: [installation, upgrade, migration, releases]
---

This page lists the actions required when upgrading stageset-controller. Each
release that needs action gets a section below, headed by its calendar version; a
release with no section needs no migration — a plain upgrade (bump the chart's
`appVersion` or pull the new image tag) suffices.

## After 2026.7.31184504

Five gates changed. Two now cover ground they were documented to cover and did
not, so a rollout they used to let through can stop; two became more forgiving,
so a rollout they used to block can proceed; one refuses a configuration it
previously accepted and silently stalled on. Nothing needs action before
upgrading, but three are worth checking against your specs first.

### Image policies: `skip` no longer disarms other policies

An image named under any matching `ImageVerificationPolicy`'s `skip` used to be
exempt from **every** matching policy. A `skip` now exempts an image only from
the policy that declares it; another policy naming the same image under `images`
still verifies it.

Where several cluster-scoped policies overlap, an image that was passing
unverified may now be verified — and hold its stage under
[`ImageUnverified`](/runbooks/imageunverified/) if it has no signature. Review
overlapping policies before upgrading:

```shell
kubectl get imageverificationpolicies -o custom-columns=NAME:.metadata.name,IMAGES:.spec.images,SKIP:.spec.skip
```

An image every matching policy skips stays exempt, including under
`--require-image-verification`.

### Migration ladders: `down` actions count towards the http allowlist

A ladder from `spec.migrationsSourceRef` may only use `http` actions when the
controller runs with `--allowed-action-hosts`. That check previously read only
each migration's `actions`, so a ladder whose http call sat in `down` was
accepted. It now reads both.

A sourced ladder with an http action in `down`, on a controller with no
allowlist, is refused with `InvalidSpec`. Set `--allowed-action-hosts` to the
hosts those actions legitimately call.

### Rollback verifies images

A rollback re-applies through the image-verification gate, so a revert can no
longer land an image the forward apply would have refused. A previous revision
whose images no longer verify reports
[`ImageUnverified`](/runbooks/imageunverified/) instead of reverting; the stage
holds at its failed state rather than reverting to something unverified.

### Restart gates measure since the last promotion

`promotion.restartGate` compared `maxRestarts` against the lifetime restart
totals of the pods it watches. Pods routinely outlive a rollout, so a workload
carrying restarts from weeks earlier failed the gate the next time it shipped —
permanently, since a lifetime total never falls.

The gate now counts only restarts observed since the stage was last promoted,
recorded in `status.stages[].promotionState.restartBaseline`. A stage stuck on
this reason clears on the next reconcile after upgrading. A stage that legitimately
breached still breaches. No spec change is needed.

### CEL ready-checks honour `inProgress`

`readyChecks.exprs[].inProgress` was accepted and ignored. It now decides what a
verify timeout means: an object that is not `current` but still reports
`inProgress` holds the stage under
[`StageProgressing`](/runbooks/stageprogressing/) and is re-verified, instead of
failing it — and, with `spec.rollbackOnFailure`, instead of reverting an install
that was going to succeed.

A check that declares no `inProgress` keeps the old behaviour, so anything
matching on `StageFailed` only sees less where you have written one. Set
`onTimeout: Rollback` on the stage to keep reverting on timeout.

### Metric sources apply their guards to redirects

`--allowed-action-hosts` was checked against the address a metric source names
and then not again, so a permitted source could redirect the query to any host
the SSRF guard allows. Every hop is now checked, which bounds where a query ends
up rather than only where it starts. This covers `spec.errorBudget`, a stage's
`promotion.analysis` and `promotion.fastTrack`, and FleetRollout wave gates.

If you run with an allowlist and a source legitimately redirects — a load
balancer sending the query to a regional endpoint, say — add the destination host
alongside the entry one.

A query carrying a `secretRef` bearer token now refuses a redirect to a different
host outright, allow-listed or not. Go forwards `Authorization` to a subdomain of
the original, so the allowlist alone could not keep a tenant's token off a host
nobody named. Redirects within the same host still carry it. A source that
depended on a cross-host redirect to deliver its token was already only working
by that quirk; point the address at the final host instead.

### FleetRollout wave gates

A wave `gate` is now bounded by `--allowed-action-hosts`, like every other
outbound call the controller makes. If you run with an allowlist, add the host
your fleet gates query or they will report the source as unavailable and hold.

A gate whose `source` names a `secretRef` is now refused with `GateUnsupported`.
That credential could never be resolved — a FleetRollout is cluster-scoped, so
there is no namespace to read the Secret from — and the rollout used to stall
with nothing on its condition explaining why. Expose the fleet-wide metric
without authentication, or move the authenticated query to a StageSet-level
source, which has both a namespace and a ServiceAccount to read it under.

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
