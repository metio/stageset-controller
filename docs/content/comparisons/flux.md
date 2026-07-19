---
title: StageSet vs Flux kustomize-controller
description: Intra-release stages and gates versus per-Kustomization ordering.
tags: [comparison, flux, stages]
---

This is the closest comparison — `StageSet` is built *for*
[Flux](https://fluxcd.io/) and borrows its conventions. Flux's `kustomize-controller`
(and `helm-controller`) reconcile a source into the cluster continuously, exactly
like `StageSet`. The difference is granularity.

## What kustomize-controller gives you

- Continuous reconciliation of a `Kustomization` from a Flux source, with pruning,
  health checks, drift correction, and `dependsOn` ordering **between**
  Kustomizations.
- Impersonation via `serviceAccountName`, `postBuild` substitution, patches — the
  same surface `StageSet` deliberately mirrors.

## Where StageSet differs

- **Ordering within a release.** `kustomize-controller` applies one Kustomization
  as a unit; ordering exists only *between* Kustomizations via `dependsOn`. To
  sequence three steps you create three Kustomizations and wire their
  dependencies. `StageSet` expresses that as one resource with ordered `stages` —
  and the controller waits for each stage's health before the next.
- **Typed actions between steps.** Migrations, HTTP gates, waits, and transient
  applies are first-class [actions](/defining-a-release/actions/); in plain Flux you'd model
  these as extra Kustomizations and Jobs.
- **Typed dependencies.** `dependsOn` exists in Flux too, but only as readiness
  ordering. `StageSet` adds a `minVersion` floor — a dependent waits until its
  dependency is deployed *at or above* a version, not merely Ready, so "the app rolls
  only once the database is migrated to 5.2" is a declared gate, not a hope.
- **Release-level features.** [Update windows](/gating/update-windows/),
  [versioned migrations](/gating/versioned-migrations/), and
  [rollback](/gating/rollback/) operate across the whole staged release.
- **Source-native.** A stage consumes a `GitRepository`/`OCIRepository`/`Bucket`
  directly (just like `kustomize-controller`), or an `ExternalArtifact` (RFC-0012),
  or a *producer* resolved to its artifact — which is how it also pairs with
  renderers like [JaaS](https://jaas.projects.metio.wtf/).
- **SOPS parity.** Encrypted Secrets in a source decrypt the same way, via
  [`spec.decryption`](/security/encryption/) (age, PGP, or cloud KMS), so a SOPS-using
  repo ports across unchanged.

## Using them together

`StageSet` sits alongside the other Flux controllers and reuses Flux's source layer,
notifications (`Alert`/`Provider` targeting `kind: StageSet`), and reconcile
annotations. Use `kustomize-controller` for ordinary one-shot reconciliation and
reach for `StageSet` when a release needs ordered, gated stages.
