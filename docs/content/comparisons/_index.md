---
title: Comparisons
description: How StageSet relates to Helm, Kustomize, Flux, Tanka, kubecfg, Argo Rollouts, jsonnet-controller, and the Flux Operator ResourceSet.
tags: [comparisons, helm, kustomize, flux, argo-rollouts]
---

`StageSet` isn't a templating tool and isn't a replacement for your manifest
generator. It's a *delivery* controller: it takes manifests that already exist
(as a [Flux](https://fluxcd.io/) `ExternalArtifact`) and rolls them out in order,
with gates, under continuous reconciliation. The tools below come up in the same
situations; here is how each one relates.

| | Generates manifests | Applies them | Continuous reconcile / drift | Ordered stages within a release | Gates / typed actions |
|---|---|---|---|---|---|
| **StageSet** | no | yes | yes | **yes** | **yes** |
| Helm | yes (templates) | yes (`helm upgrade`) | no | hooks + weights | hooks only |
| Kustomize (`kustomize` CLI) | yes (overlays) | no (`kubectl apply`) | no | no | no |
| Flux `kustomize-controller` | no | yes | yes | between Kustomizations | health checks |
| Tanka / kubecfg | yes (Jsonnet) | yes (CLI) | no | dependency order | no |

`StageSet` is complementary to all of them. It consumes manifests produced by
[Helm](https://helm.sh/), [Kustomize](https://kustomize.io/),
[Tanka](https://tanka.dev/), or anything else — its job starts once you have
manifests and need to deliver them carefully.

Progressive-delivery controllers ([Flagger](https://flagger.app/),
[Argo Rollouts](https://argoproj.github.io/argo-rollouts/)) sit at another layer —
traffic shifting for a single workload — and also compose with `StageSet` rather
than replace it; see [vs Argo Rollouts](/comparisons/argo-rollouts/).

## All comparisons

- **[vs Helm](/comparisons/helm/)** — templating and release hooks vs ordered, gated delivery.
- **[vs Kustomize](/comparisons/kustomize/)** — overlay rendering vs continuous, staged apply.
- **[vs Flux kustomize-controller](/comparisons/flux/)** — one-shot reconciliation vs sequenced stages with gates.
- **[vs Tanka and kubecfg](/comparisons/tanka-kubecfg/)** — Jsonnet rendering vs delivery of the rendered result.
- **[vs Argo Rollouts](/comparisons/argo-rollouts/)** — single-workload traffic shifting vs release-wide staging.
- **[vs jsonnet-controller](/comparisons/jsonnet-controller/)** — in-cluster Jsonnet apply vs the producer/delivery split.
- **[vs Flux Operator ResourceSet](/comparisons/flux-operator/)** — horizontal fan-out vs vertical, gated promotion.
