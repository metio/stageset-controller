---
title: StageSet vs Helm
description: How StageSet relates to Helm's templating and hook ordering.
tags: [comparison, helm, stages]
---

[Helm](https://helm.sh/) is two things: a templating engine (charts) and an
imperative release tool (`helm upgrade`). `StageSet` is neither — it's a declarative
delivery controller. The overlap is ordering: Helm's hooks and hook weights give you
*some* sequencing inside a single chart's install/upgrade.

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
- **Typed gates between steps.** Hooks run Jobs; `StageSet` stages can run Jobs,
  HTTP gates, waits, patches, deletes, and transient applies, as pre/post/onFailure
  [actions](/usage/actions/).
- **Identity.** A `StageSet` applies under a per-tenant `ServiceAccount`; `helm
  upgrade` runs as whoever ran it.

## Using them together

Render a chart to manifests (e.g. via a producer that publishes an
`ExternalArtifact`) and deliver it with `StageSet`. The controller understands
`helm.sh/hook` resources: `applyHelmHookResources` (default `true`) applies them as
ordinary objects, so a Helm-style chart's hook resources still get created — now
under `StageSet`'s ordering and gating instead of Helm's.
