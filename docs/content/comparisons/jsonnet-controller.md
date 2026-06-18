---
title: StageSet vs jsonnet-controller
description: A jsonnet-native Flux applier versus a renderer-agnostic staged-delivery layer.
tags: [comparison, jsonnet, flux, stages]
---

[jsonnet-controller](https://github.com/pelotech/jsonnet-controller) (pelotech) is a
Flux controller that evaluates Jsonnet (kubecfg- and Tanka-style) and applies the
result to the cluster. Its `Konfiguration` resource (`jsonnet.io/v1beta1`) is, in
effect, *kustomize-controller for Jsonnet*: point it at a `GitRepository` (or an
HTTP(S) URL), and it builds the Jsonnet and reconciles the manifests — with
pruning, health/revision tracking, TLA string/code variables, and `dependsOn`
ordering **between** Konfigurations.

The two projects sit at **different layers**, which is the whole comparison.

## What jsonnet-controller gives you

- **Jsonnet rendering and applying in one resource.** A `Konfiguration` both
  evaluates the Jsonnet and applies the output — the rendering engine is part of
  the controller. If your goal is "get this Jsonnet/Tanka tree into the cluster,"
  it's a direct, single-resource answer.
- The familiar Flux applier surface: prune, health, interval reconciliation, TLAs,
  and `dependsOn` between Konfigurations.

## Where StageSet differs

- **Renderer-agnostic.** `StageSet` does *not* evaluate Jsonnet. A stage consumes a
  Flux source — `GitRepository`, `OCIRepository`, `Bucket`, or an `ExternalArtifact`
  — so it rolls out plain manifests *or* the output of any renderer. For Jsonnet
  specifically, [JaaS](https://jaas.projects.metio.wtf/) does the evaluation
  (TLAs, ext vars, jb-vendored libraries, [JOI](https://github.com/metio/jsonnet-oci-images)
  images) and publishes an `ExternalArtifact` that `StageSet` consumes. Rendering and
  delivery are separate concerns, owned by separate components.
- **Ordering and gating *within* a release.** A `Konfiguration` applies as one unit;
  sequencing exists only *between* Konfigurations via `dependsOn`. `StageSet` expresses
  a release as ordered [stages](/usage/stages-and-sources/), each waiting on the
  previous stage's health, with typed [actions](/usage/actions/) (migration Jobs,
  HTTP gates, waits) *between* steps.
- **Release-level machinery** jsonnet-controller doesn't carry:
  [update windows](/usage/update-windows/),
  [versioned migrations](/usage/versioned-migrations/),
  [conflict policies](/usage/conflict-policies/), and
  [rollback](/usage/rollback/) across the whole staged release.

## Which to reach for

- You want **Jsonnet rendered and applied as a single unit**, no staging →
  jsonnet-controller is a clean fit (as is JaaS paired with Flux's
  `kustomize-controller`).
- You want **ordered, gated, multi-stage delivery** of manifests — whatever renders
  them → `StageSet`, with `JaaS` supplying the Jsonnet rendering when you need it.

They are not mutually exclusive: jsonnet-controller answers *how Jsonnet becomes
manifests*, `StageSet` answers *how a release is sequenced and gated*. You can pick a
renderer independently — `JaaS` is one option that keeps the rendering reusable
(local `jsonnet`-parity, OCI libraries) and hands `StageSet` an artifact like any
other.

## Using them together

Because a `Konfiguration` is a Kubernetes object, a `StageSet` stage can **apply a
`Konfiguration` and gate on it** — letting jsonnet-controller do the Jsonnet
rendering and applying while `StageSet` sequences it among other stages, with actions
and gates in between.

Put the `Konfiguration` manifest in a source the stage reads (here a
`GitRepository`), then gate the stage on the `Konfiguration` reaching `Ready` for
its current generation:

```yaml
apiVersion: stages.metio.wtf/v1
kind: StageSet
metadata:
  name: platform
  namespace: platform
spec:
  stages:
    - name: base                     # render + apply the Jsonnet via jsonnet-controller
      sourceRef:
        kind: GitRepository
        name: platform-konfig        # a repo holding the Konfiguration manifest
      readyChecks:
        exprs:
          - apiVersion: jsonnet.io/v1beta1
            kind: Konfiguration
            # don't proceed until jsonnet-controller has reconciled THIS generation
            current: "status.observedGeneration == metadata.generation && status.conditions.exists(c, c.type == 'Ready' && c.status == 'True')"
            inProgress: "status.conditions.exists(c, c.type == 'Ready' && c.status == 'Unknown')"
            failed: "status.conditions.exists(c, c.type == 'Ready' && c.status == 'False')"
    - name: smoke                     # only runs once the Konfiguration is Ready
      sourceRef:
        kind: GitRepository
        name: platform-smoke
```

`StageSet` applies the `Konfiguration` and waits — the `current` expression holds the
rollout until jsonnet-controller has observed the latest generation *and* reports
`Ready`, so a later stage never starts against a half-rendered base.

### Ownership differs from the JaaS path

This is the important distinction, and it changes who prunes what:

- **Via jsonnet-controller (above).** `StageSet`'s inventory owns **only the
  `Konfiguration` object**. The workloads rendered from the Jsonnet are owned and
  **pruned by jsonnet-controller** — `StageSet` never sees them individually. Delete
  the stage and `StageSet` removes the `Konfiguration`; jsonnet-controller then
  cascades the prune of what it created. You get two nested owners.
- **Via [JaaS](https://jaas.projects.metio.wtf/) (or any source).** `JaaS` only
  *renders* — it publishes an `ExternalArtifact` and owns nothing in the target
  cluster. `StageSet` fetches that artifact and applies the manifests itself, so
  **`StageSet`'s inventory owns every rendered object directly** and prunes them from
  its own `StageInventory` record. One owner, and `StageSet`'s drift correction,
  [conflict policies](/usage/conflict-policies/), and [rollback](/usage/rollback/)
  apply to the resources themselves.

So if you want `StageSet` to be the single owner and pruner of the delivered
resources, render with `JaaS` (or read plain manifests from a source). Reach for the
`Konfiguration`-as-a-stage pattern when you specifically want jsonnet-controller to
keep owning what it renders, and only need `StageSet` to sequence and gate it.
