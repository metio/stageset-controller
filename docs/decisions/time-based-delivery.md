# Time-based delivery and maintenance windows

## Decision

A StageSet may declare **update windows** that gate when new revisions roll out:

```yaml
spec:
  windowScope: Updates        # Updates (default) | All
  updateWindows:
    - type: Allow             # Allow | Deny
      schedule: "0 2 * * 2,4" # cron start; 02:00 Tue & Thu
      duration: 2h
      timeZone: Europe/Berlin
    - type: Deny              # absolute one-off freeze
      from: "2026-12-20T00:00:00Z"
      to:   "2027-01-02T00:00:00Z"
```

Evaluation (Argo-CD-sync-window semantics): a **Deny** active now blocks; if any **Allow** windows are declared, updates are allowed only while an Allow is active and no Deny is. No windows means always allowed (today's behaviour). Each window is either *recurring* (`schedule` + `duration`) or *absolute* (`from`/`to`), with a per-window IANA `timeZone` (default UTC).

`windowScope` chooses what a closed window blocks:

- **`Updates`** (default) — only a **new-revision rollout** is held. Drift correction of the *current* deployed revision still runs, so the deployed contract is maintained while the cluster is "frozen for new versions".
- **`All`** — a hard freeze: even drift correction is paused while a window is closed ("don't touch my cluster").

When delivery is held, `status.pendingUpdate` records the held revisions and the next time a window opens; the controller requeues at the next window boundary. An emergency **`stages.metio.wtf/update-now`** annotation forces the held rollout through once, regardless of windows.

## Context

Flux's time-based delivery (flux-operator reacting to an OCIRepository) gates at the **source**. When one shared source feeds many tenants, that imposes a single schedule on every consumer. The unit of "when may this change" is the **tenant's workload**, not the artifact — so the gate belongs on the StageSet that deploys it. The two layers compose rather than compete: the producer publishes whenever it likes; each StageSet *applies* only when its own window allows.

Neither Flux `Kustomization` nor `HelmRelease` offers per-workload time-based delivery, so this is also a genuine capability gap. The closest mature precedent is Argo CD's sync windows (allow/deny, cron, duration, timezone, manual override), whose model we adopt and extend with absolute date-range windows.

## Why consumer-level, not source-level

A shared OCIRepository of common manifests is consumed indirectly: gating its publication would force every tenant onto one maintenance window. Tenants legitimately want different windows for the same content — staging rolls continuously, production only at 02:00, the payments namespace freezes over the holidays. Putting the window on the StageSet makes the window a property of the *tenant/workload*, which is where the requirement actually lives.

## Semantics in detail

- **What "new revision" means.** After resolving each stage's artifact, the controller compares the resolved revision to `status.lastAppliedRevisions`. Any difference (including the first-ever apply) is a rollout; identical revisions are steady-state.
- **Held rollout, already deployed.** `Ready` stays **True** (the deployed state is healthy and unchanged) with a message noting the deferral; `status.pendingUpdate` carries the held revisions and `nextWindowOpens`. A held rollout is a deliberate wait, not a failure — making it `Ready=False` would page operators for normal scheduling.
- **Held rollout, never deployed.** With nothing yet applied, `Ready` is **False** with reason `UpdateDeferred` and the same `pendingUpdate` detail — the workload genuinely is not ready.
- **`windowScope: All` freeze with no pending change.** The reconcile short-circuits before applying; `Ready` keeps its prior value and `pendingUpdate.nextWindowOpens` shows when the freeze lifts.
- **Latest-at-window, not pinned.** A held StageSet does not pin the revision it first saw; when the window opens it applies whatever is current then, so tenants get the newest content at delivery time.
- **Override.** `stages.metio.wtf/update-now` forces the held rollout regardless of windows, one-shot per annotation value (tracked in `status.lastHandledUpdateOverride`, the same token mechanism as `reconcile.fluxcd.io/requestedAt`). It is deliberately distinct from a reconcile request, so "reconcile now" never silently means "ignore the freeze".
- **Requeue.** When held, the controller requeues at the next window boundary (capped so a multi-week absolute freeze re-checks periodically rather than sleeping blindly across clock changes).
- **Clock.** Window evaluation uses the controller's clock; under leader election a single replica decides. Timezones use the IANA database, which the chainguard `static` runtime image does not ship — so the binary embeds it (`import _ "time/tzdata"`).

## Composition

- **`dependsOn`** — a held-but-deployed StageSet stays `Ready`, so dependents proceed against its current state and roll forward in *their own* windows.
- **Migrations** fire only on a version transition, which is exactly what a closed window holds — so versioned migrations naturally run during the maintenance window, not before it.
- **Flagger** is orthogonal: windows decide *when a rollout may start*; Flagger shapes traffic *within* a rollout.

## Scope and alternatives

- **Inline only (v1).** Windows live on the StageSet. A shared, referenced `MaintenanceWindow` CRD (cluster- or namespace-scoped, for platform-defined schedules many tenants opt into) is a natural fast-follow, deferred until there is demand — inline covers the per-tenant requirement directly.
- **Per-stage windows** were considered and deferred: per-StageSet is the requirement, and per-stage windows would multiply the deferral states without a concrete need yet.
- **Recurring vs absolute.** Supporting both avoids forcing one-off holiday freezes into annual cron, and avoids forcing weekly windows into enumerated dates.

## New wire-stable surfaces

- `spec.updateWindows`, `spec.windowScope`, `status.pendingUpdate`, `status.lastHandledUpdateOverride`
- the `stages.metio.wtf/update-now` annotation
- the `UpdateDeferred` Ready reason
