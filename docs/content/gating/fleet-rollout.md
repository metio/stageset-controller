---
title: Fleet rollout
description: Roll one version across a fleet of StageSets in ordered waves — canary a few tenants, soak, health-check, then widen, and halt the whole fleet on regression.
tags: [fleet, progressive-delivery, waves, versioning, rollback, multi-tenancy]
---

When you run one product across many StageSets — one per tenant, often one per
namespace — a new version reaches every tenant at once. A `FleetRollout` paces that:
it rolls a version across a selected set of StageSets in **ordered waves**, opening
the next wave only after the current one reaches the version, stays healthy through a
soak and a metric gate, and **halts the whole fleet** the moment a wave regresses.

A `FleetRollout` is cluster-scoped — a fleet spans namespaces. It **approves** the
version each member's own source already offers; it does not push versions, so it
composes with GitOps rather than bypassing it. By default it does not even name the
version — it **derives** each member's target from the advance that member is already
holding, so one `FleetRollout` becomes a standing wave policy that paces every future
version its members' sources publish, with nothing to keep in sync.

## Make members fleet-managed

A member holds each version advance for the fleet to approve. Set
[`approvalMode: Always`](/gating/versioned-migrations/#holding-a-transition-for-approval)
on every StageSet in the fleet:

```yaml
apiVersion: stages.metio.wtf/v1
kind: StageSet
metadata:
  name: app
  namespace: moodle-acme
  labels:
    app: moodle          # the fleet selects on this
    ring: "1"            # the wave this tenant belongs to
spec:
  version:
    fromObject: { stage: app }
    approvalMode: Always
    allowDowngrade: true # only needed if the fleet may roll back (see below)
  # …
```

Without `approvalMode: Always` a member advances on its own and the fleet cannot
pace it.

## Define the rollout

Label your tenants into rings, then roll wave by wave:

```yaml
apiVersion: stages.metio.wtf/v1
kind: FleetRollout
metadata:
  name: moodle
spec:
  # No targetVersion: the fleet derives it from each member's own held advance.
  selector:
    matchLabels: { app: moodle }          # the whole fleet
  namespaceSelector:                       # optional; omit to span every namespace
    matchLabels: { tenant: "true" }
  waves:
    - name: canary
      selector: { matchLabels: { ring: "0" } }
      soak: 30m
      gate:                                # health check before the next wave opens
        source:
          prometheus:
            address: http://prometheus.monitoring:9090
            query: 'sum(rate(http_requests_total{app="moodle",code=~"5.."}[5m]))'
        threshold: { max: "0.01" }
    - name: broad
      selector: { matchLabels: { ring: "1" } }
      soak: 1h
    - name: rest
      selector: { matchLabels: { ring: "2" } }
```

Each wave is a label selector over the members. A member that matches `selector` but
no wave fails the rollout closed (`MembersUnassigned`) rather than being silently
skipped.

**Deriving vs. pinning the version.** With no `targetVersion` (above), the fleet reads
each held member's `status.pendingVersion` — the advance its own source is offering —
and approves *that*, so you never restate a version the source already declares and
the two can't drift. Set `targetVersion: "2.0.0"` only to pin a specific, bounded
rollout of exactly that version; a member whose source does not offer the pinned
version waits until it does.

## How a rollout progresses

The controller opens one wave at a time:

1. **Open** the wave — stamp the version onto each member's approval, releasing its
   held advance. Members reach the version when their own source offers it.
2. **Settle** — wait until every member of the wave has adopted its version and is
   Ready.
3. **Soak** — hold the wave's `soak` after it settles.
4. **Gate** — evaluate the wave's `gate` metric. A scalar within the threshold passes;
   one outside it halts the fleet.
5. **Advance** — open the next wave. When every wave passes, the rollout is `Completed`.

`stagesetctl fleet <name>` shows exactly where the rollout is — see
[`fleet`](/cli/fleet/).

## Pin a pilot instance first

To require one specific "smoke-test" tenant to update before anything else, give it
its own ring and make it wave 0:

```yaml
  waves:
    - name: pilot
      selector: { matchLabels: { ring: canary } }   # only the test tenant carries this
      soak: 15m
      gate: { … }                                    # only proceed if it's healthy
    - name: fleet
      selector: { matchLabels: { ring: prod } }
```

The pilot is the sole member of wave 0, so the rest of the fleet waits until it
reaches the version, soaks, and passes its gate.

## Halt on regression

A wave **halts the whole fleet** — no further waves open — when either its `gate`
metric falls outside the threshold, or a member that had reached the version goes
not-Ready (a regression). The rollout reports `phase: Halted` and `Ready=False` with
reason `Halted`, and emits a Warning event naming the cause. A halt is re-derived
each reconcile, so it clears on its own once the cause does — a member recovers, or
the gate passes again. A metric the controller cannot read holds the wave (neither
advancing nor halting) rather than guessing.

## Roll back a regressed wave

By default a regression only halts. Set `onRegression: Rollback` with a
`previousVersion` to also **revert** the affected wave:

```yaml
spec:
  targetVersion: "2.0.0"
  previousVersion: "1.0.0"     # required for Rollback
  onRegression: Rollback
  # …
```

On a regression the fleet directs the halted wave's members back to `previousVersion`
— it stamps a `rollback-to` annotation that overrides each member's source-resolved
version with the lower one, unwinding it through the migration
[`down` actions](/gating/versioned-migrations/#rolling-back). A member only actually
reverts if it permits downgrades (`spec.version.allowDowngrade`) and the crossed
migrations declare `down` actions; otherwise it refuses, and the fleet stays halted
and says so. Unlike a manifest-only rollback, a member either unwinds its schema
cleanly or refuses — never leaves the schema ahead of the code.

## One rollout per StageSet

A StageSet must be governed by at most one `FleetRollout` — two would both stamp its
approval and fight over it. When a member is selected by more than one rollout, the
rollout fails closed with `Ready=False`, reason `MembersContested`, naming the other
rollout, and stamps nothing until the overlap is resolved.

## Status reasons

The rollout's `Ready` condition carries one of:

| Reason | Meaning | What to do |
|---|---|---|
| `Progressing` | A wave is open and advancing. | Nothing — watch with `stagesetctl fleet`. |
| `Completed` | Every wave reached the target version. | Nothing. |
| `Halted` | A gate failed or a member regressed. | Investigate the named wave; the halt clears when the cause does. |
| `MembersUnassigned` | A selected StageSet matches no wave. | Add the missing ring label, or a wave that selects it. |
| `MembersContested` | A member is also in another rollout. | Narrow one rollout's selector so each member has one owner. |
| `InvalidSelector` | A selector is malformed. | Fix the selector. |

A member held awaiting its wave reports its own `Ready=False` with reason
[`AwaitingApproval`](/runbooks/awaitingapproval/) — that is the normal held state, not
an error.
