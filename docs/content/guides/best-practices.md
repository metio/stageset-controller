---
title: Best practices
description: How to get the most out of StageSet — keep the render pure, model the release as gated stages, choose action scopes deliberately — and the anti-patterns that bite.
tags: [guides, best-practices, actions, stages]
---

StageSet works best when you lean into what it is: a delivery controller that
keeps the *render* pure and owns *ordering, gating, and execution memory*. Almost
every practice below — good or bad — follows from respecting or fighting that
split. The one-line test for a decision is: **does this belong in what should
exist (the render) or in when and whether it runs (StageSet)?**

For a single application, a small team, or a chart you distribute to others,
Helm's simplicity and ecosystem may serve you better; see
[when Helm is the better choice](#when-helm-is-the-better-choice). These practices
assume the case StageSet is built for — a multi-component, multi-tenant, stateful,
policy-gated release.

## Practices that make StageSet shine

### Keep manifest generation a pure function

Let your renderer — Jsonnet, Helm, Kustomize — answer only *what should exist*,
and keep it free of lookups, side effects, or "if already installed then…" logic.
Delivery order, health gating, and "have I done this before?" belong in the
StageSet, where the controller can see and gate them. A template that reads
cluster state or branches on it relocates execution memory into a place StageSet
can neither observe nor sequence.

### Model the release as staged, gated steps

Split a release into ordered [stages](/defining-a-release/stages-and-sources/) —
CRDs, then the operator that needs them, then the app — rather than one artifact
whose internal ordering you have to reconstruct later. Hold each stage with a
[ready check](/defining-a-release/ready-checks/) that proves it is genuinely
*healthy* (kstatus, or a CEL expression on the object's status), not merely
*applied*. "Applied" is not "ready": a stage that reports done the moment its
manifests land lets the next stage start against a dependency that isn't up yet.

### Choose the action scope that matches its lifetime

An action's [`scope`](/defining-a-release/actions/#scope-revision-version-or-lifetime)
declares how often it should run. Pick the *lightest* one that fits:

```yaml
# on a stage of a versioned StageSet (spec.version is set)
actions:
  pre:
    - name: render-check         # idempotent and cheap — safe to re-run
      scope: Revision
      job: { sourceRef: { name: app-check } }
    - name: db-upgrade           # upgrade choreography — once per version
      scope: Version
      job: { sourceRef: { name: app-migrate } }
  post:
    - name: provision-object-store  # once ever; external effect, so no anchor
      scope: Lifetime
      job: { sourceRef: { name: app-provision-store } }
```

An action whose effect lives *in* the cluster — a database on a bundled PVC —
takes a [`completionAnchor`](#anchor-a-once-ever-action-to-the-state-it-creates)
instead, covered next.

- **`Revision`** (the default) for idempotent, per-config work — a render check, a
  cache warm. It re-runs on any new revision, which is fine when the action is cheap
  and safe.
- **`Version`** for upgrade choreography — a schema migration, a maintenance-mode
  toggle. Keyed to the resolved `spec.version`, so a config-only change at a fixed
  version stops re-firing the whole ladder. (`Version` requires `spec.version`.)
- **`Lifetime`** only for genuinely once-ever bootstraps — an install, a seed. It
  is the heaviest tool: its completion lives in a durable
  [`StageLedger`](/defining-a-release/actions/#run-once-ever-scope-lifetime) that
  survives a delete-and-recreate. Most "run once" needs are really `Version` or a
  [versioned migration](/gating/versioned-migrations/); reach for `Lifetime` last.

### Anchor a once-ever action to the state it creates

When a `Lifetime` action's effect lives in an object the stage manages — a
database PVC, a `Database` CR — name it as a
[`completionAnchor`](/defining-a-release/actions/#tie-a-completion-to-a-witness-with-completionanchor)
on a **`post`** action:

```yaml
post:
  - name: install-database
    scope: Lifetime
    completionAnchor:
      apiVersion: v1
      kind: PersistentVolumeClaim
      name: app-db
    job: { sourceRef: { name: app-install } }
```

The completion is then valid only while that object exists with the UID recorded
at completion, so pruning the PVC (or recreating it empty) re-runs the bootstrap —
no coordination code. Leave an action *un*-anchored only when its effect is
genuinely external and permanent: a schema in an external database, an object-store
bucket.

### Make actions idempotent anyway

The ledger is the primary guard against a double-run; write actions so a *retry*
is still safe — `CREATE TABLE IF NOT EXISTS`, an installer that checks for its own
prior state. A `Lifetime` action can still be retried on failure, or deliberately
re-run with [`reset-ledger`](/cli/reset-ledger/); the scope reduces re-runs, it
does not make a destructive action safe on its own.

### Track the version from what is deployed

Derive `spec.version` from the running object with `version.fromObject` rather than
hand-maintaining a field, so `scope: Version` and
[versioned migrations](/gating/versioned-migrations/) key off what is actually
deployed instead of drifting from it.

### Gate with the built-in controls

Reach for the release-level gates instead of pausing by hand or bolting on external
orchestration: [update windows](/gating/update-windows/) for change freezes,
[error-budget freeze](/gating/error-budget/) to hold rollouts while a service is
out of SLO, and [promotion gates](/gating/stage-promotion/) (soak, manual approval,
metric analysis) between stages.

### Keep the controller least-privileged

Run every apply as a per-tenant `ServiceAccount`
([multi-tenancy](/security/multi-cluster/)) so a StageSet can only touch what its
tenant's RBAC allows, and keep the controller's own grants minimal — `cluster-admin`
is for single-tenant installs only.

### Keep the ledger honest: adopt, and back it up

To bring a running system under StageSet without re-running its bootstrap, assert
the completion with [`stagesetctl baseline`](/cli/baseline/) — a committable
`spec.baseline` entry. And because controller-run completions live only in etcd,
export them as part of your backup routine so a
[disaster-recovery](/running/disaster-recovery/) rebuild from Git does not re-seed
a database that survived the outage.

## Practices that hurt users

- **Imperative logic in the renderer.** A "does the world look done?" probe in a
  template lies to you: a restored backup or a half-crashed install reads as
  *not done* and diverges on re-run. "Did I do X" beats "does X look done".
- **The wrong scope.** `Lifetime` on a per-upgrade action means it never re-runs at
  the next version; `Version` or `Lifetime` on a per-config check means it stops
  validating after the first run. A mismatched scope fails *silently* — the action
  runs the wrong number of times, with nothing to flag it.
- **Anchoring a routinely-recreated object.** A `completionAnchor` on a Deployment
  or Pod flaps every rollout, re-running the bootstrap each time. Anchor only
  state-bearing objects (a PVC, a database CR).
- **Anchoring a stage-applied object on a `pre` action.** Pre-actions run before the
  apply, so the anchor does not exist yet and the action fails at completion. Put it
  on a `post` action.
- **Deleting a StageSet to "reset" a system.** Expecting a clean reinstall (the Helm
  reflex) backfires: the ledger is retained, the `Lifetime` bootstrap is skipped,
  and the app comes up against stale or empty state. Watch for the `LedgerAdopted`
  event, and use [`reset-ledger`](/cli/reset-ledger/) when you *mean* to forget.
- **`prune: false` as a habit.** Disabling pruning to "protect" objects leaves
  silent orphans and breaks the [inventory](/defining-a-release/inventory/) diff.
  Use it only for the deliberate orphan-on-purpose case, knowing the trade-off.
- **Cramming a release into one stage.** A single stage with a wall of manifests is
  back to hook-style opacity — you lose the ordering and gating that are the reason
  to use StageSet at all.
- **A broad controller ClusterRole in multi-tenant clusters.** Granting the
  controller `cluster-admin` defeats the per-tenant `ServiceAccount` isolation; let
  each tenant's RBAC bound what its StageSets can touch.
- **Skipping the ledger backup.** Not committing `spec.baseline` and not exporting
  completions turns a Git-based disaster recovery into a bootstrap re-run against
  live state.

## When Helm is the better choice

None of this makes StageSet the right tool everywhere. For a single application, a
small team, or a chart you package and distribute to others, Helm's ubiquity,
ecosystem, and operational minimalism win — and the two
[compose](/comparisons/helm/): render a chart to manifests and deliver it with
StageSet when a release needs ordered, gated stages and a durable action lifecycle.
Reach for StageSet when the release is multi-component, the fleet is multi-tenant,
the workloads are stateful, and rollout policy is a requirement — the case its
complexity pays for.
