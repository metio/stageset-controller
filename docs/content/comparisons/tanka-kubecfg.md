---
title: StageSet vs Tanka and kubecfg
description: Reconciled, gated delivery versus CLI-driven Jsonnet apply.
tags: [comparison, tanka, jsonnet, stages]
---

[Tanka](https://tanka.dev/) and [kubecfg](https://github.com/kubecfg/kubecfg) are
Jsonnet-based config tools: you express your resources in Jsonnet, the tool renders
them, diffs against the cluster, and applies. They generate configuration and run a
CLI-driven apply, but they are imperative tools you run, not controllers that
reconcile.

## What Tanka / kubecfg give you

- Jsonnet-powered, DRY manifest generation (libraries, abstractions, environments).
- A `diff`/`apply` workflow with dependency-aware ordering of a single apply.

## Where StageSet differs

- **Reconciliation, not invocation.** Tanka/kubecfg apply when *you* run them.
  `StageSet` runs in-cluster and continuously reconciles, corrects drift, and
  prunes.
- **Staged, gated delivery.** They apply a rendered set (in dependency order);
  they don't model multi-stage rollouts with readiness gates, update windows, or
  versioned migrations between stages.
- **GitOps identity and tenancy.** `StageSet` applies under an impersonated tenant
  `ServiceAccount` inside the cluster; Tanka/kubecfg use your local credentials.

## Using them together

The Jsonnet *generation* that Tanka and kubecfg do so well has a GitOps-native
equivalent in two related projects:

- **[JOI](https://github.com/metio/jsonnet-oci-images)** ships the Jsonnet
  libraries as OCI images.
- **[JaaS](https://jaas.projects.metio.wtf/)** evaluates the Jsonnet in-cluster
  and publishes the rendered result as an `ExternalArtifact` — the rendering step,
  as a service.
- **`StageSet`** delivers that artifact in ordered, gated stages — the apply step,
  as a controller.

So where you might run `tk apply` or `kubecfg update` from a laptop or CI, this
approach splits the same job into a producer (`JaaS`, importing `JOI` libraries) and
a delivery controller (`StageSet`), both reconciled by Flux. You can also keep using
Tanka/kubecfg to author and publish artifacts, and let `StageSet` handle delivery.
