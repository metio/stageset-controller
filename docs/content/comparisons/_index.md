---
title: Comparisons
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
