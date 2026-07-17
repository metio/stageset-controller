---
title: StageSet vs Helm
description: Templating and install/upgrade hooks versus ordered, gated delivery with a durable action lifecycle.
tags: [comparison, helm, stages]
---

[Helm](https://helm.sh/) is two things: a templating engine (charts) and an
imperative release tool (`helm upgrade`). `StageSet` is neither — it's a declarative
delivery controller. They overlap on one axis: sequencing work around a release.
Helm expresses that with hooks and hook weights *inside* a single chart's install
or upgrade; `StageSet` expresses it as ordered, gated stages with typed actions,
under continuous reconciliation.

Helm stays the better tool for what it was built for: packaging an application,
parameterizing it with values, and distributing that chart to others. `StageSet`
starts once you have manifests and need to deliver them carefully — so the two
[compose](#using-them-together) rather than compete.

## What Helm gives you

- Templated, parameterized manifests (charts and values).
- Install/upgrade ordering via `helm.sh/hook` (pre-install, post-upgrade, …) and
  `hook-weight`.
- A release history you can roll back to with `helm rollback`.

## Where StageSet differs

- **Continuous reconciliation.** `helm upgrade` is a point-in-time, imperative
  action; nothing re-asserts the state afterward. `StageSet` reconciles on an
  interval, corrects drift, and prunes — it's GitOps, not a one-shot.
- **Ordering across artifacts, not just within one chart.** Helm hooks order
  resources *inside* a release. `StageSet` orders whole *stages*, each its own
  artifact, with readiness gating between them.
- **Typed, legible gates.** A hook is an opaque Job triggered by an annotation;
  nothing surfaces what it will do or in what order. `StageSet` actions are typed —
  Jobs, HTTP gates, waits, patches, deletes, transient applies — declared in list
  order as pre/post/onFailure [actions](/defining-a-release/actions/), with failure
  a modeled state (`onFailure`, `onRollback`, automatic rollback) rather than a
  stranded hook.
- **Identity.** A `StageSet` applies under a per-tenant `ServiceAccount`; `helm
  upgrade` runs as whoever ran it.

## Run something once — and know it ran

This is the sharpest difference, and it's a capability Helm has no vocabulary for.
A Helm hook fires on a fixed schedule tied to the release operation:

- `post-upgrade` runs on **every** `helm upgrade` — including a values-only change —
  so upgrade choreography (enable maintenance mode, run the schema migration, purge
  caches) re-fires on every config push.
- `post-install` (and `.Release.IsInstall`) runs once per **install**, but a
  `helm uninstall` + reinstall is a fresh release, so it runs again — and Helm has
  to bolt on `helm.sh/resource-policy: keep` plus `--take-ownership` to stop a
  reinstall from wiping and re-seeding the state a prior install created.

`StageSet` makes that a first-class, declared property of each action — its
[`scope`](/defining-a-release/actions/#scope-revision-version-or-lifetime):

- **`Revision`** — once per delivered revision (Helm's per-operation default).
- **`Version`** — once per resolved application *version*. A config-only change at a
  fixed version never re-runs it; crossing a version boundary does. **Helm has no
  version concept for hooks at all** — this is the fix for the wart Helm users live
  with, where every `helm upgrade` re-runs the whole upgrade ladder.
- **`Lifetime`** — once for the system, *ever*. The completion is recorded in a
  durable `StageLedger` that is **not** tied to the StageSet's lifetime, so a
  delete-and-recreate does **not** re-run the bootstrap — the disaster Helm's
  `resource-policy: keep` and adopt flags exist to prevent, handled by construction
  instead of by hand. A
  [`completionAnchor`](/defining-a-release/actions/#tie-a-completion-to-a-witness-with-completionanchor)
  ties that "done" to a witness object's identity, so when the state a bootstrap
  created is actually gone — a database PVC pruned, a volume recreated empty — the
  bootstrap runs again, and only then.

Underneath the vocabulary is the real difference: a delivery controller *remembers
what it has done* and can be asked to prove it, where a package manager re-derives
its plan from the chart on each run. Adopting a system whose bootstrap already ran,
exporting that record for disaster recovery, and deliberately forgetting a
completion so it re-runs are all first-class operations
([`stagesetctl baseline`](/cli/baseline/), [`reset-ledger`](/cli/reset-ledger/)).

## Using them together

Render a chart to manifests (e.g. via a producer that publishes an
`ExternalArtifact`) and deliver it with `StageSet`. The controller understands
`helm.sh/hook` resources: `applyHelmHookResources` (default `true`) applies them as
ordinary objects, so a Helm-style chart's hook resources still get created — under
`StageSet`'s ordering, gating, and action lifecycle rather than Helm's.
