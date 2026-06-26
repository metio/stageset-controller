---
title: StageSet vs Kustomize
description: Why StageSet delivers what Kustomize only builds.
tags: [comparison, kubernetes, stages]
---

[Kustomize](https://kustomize.io/) (the `kustomize` CLI / `kubectl kustomize`) is a
manifest *builder*: it composes bases and overlays, applies patches, and emits YAML.
It does not apply anything, and it has no notion of ordering, readiness, or
reconciliation — that's `kubectl apply`'s job, and `kubectl` applies everything at
once.

## What Kustomize gives you

- Overlay composition, strategic-merge and JSON6902 patches, variable replacement,
  generators.
- A pure transformation: in goes a kustomization, out come manifests.

## Where StageSet differs

- **It delivers, not just builds.** Kustomize stops at YAML. `StageSet` applies it,
  waits for health, prunes what's gone, and keeps doing so.
- **Ordering and gates.** `kubectl apply -k` has no stages and no gates. `StageSet`
  sequences stages and runs [actions](/defining-a-release/actions/) between them.
- **Continuous reconciliation and drift correction**, versus a one-shot `apply`.

## Using them together

`StageSet` *includes* the parts of Kustomize you reach for at delivery time: a stage
has `path`, `patches`, and `postBuild` substitution
([stages and sources](/defining-a-release/stages-and-sources/)). So you can keep authoring with
Kustomize overlays and let a stage apply the right overlay, patched and
substituted — then add the ordering, gating, and reconciliation Kustomize alone
doesn't offer.
