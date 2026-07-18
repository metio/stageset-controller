---
title: StageSet vs Argo Rollouts
description: Different layers — staged multi-artifact delivery versus single-workload traffic shifting.
tags: [comparison, argo, progressive-delivery, stages]
---

[Argo Rollouts](https://argoproj.github.io/argo-rollouts/) and `StageSet` are easy
to mention in the same breath because both roll things out gradually, but they
operate at different layers and are complementary rather than competing.

## What Argo Rollouts does

`Argo Rollouts` replaces a `Deployment` with a `Rollout` that shifts traffic to a new
version **progressively** — canary or blue-green — pausing at weighted steps and
promoting based on **metric analysis** (Prometheus queries, web/Job providers).
Its unit of work is a **single workload's** version transition and the traffic in
front of it.

## What StageSet does

`StageSet` orchestrates a **whole release** as an ordered list of stages, each built
from a Flux `ExternalArtifact` — CRDs before the operator that needs them, a
migration before the app, config before the workload — gating each stage on health
and running typed [actions](/defining-a-release/actions/) between them. It does not shift
traffic or run metric analysis; its unit of work is the **multi-component release**
and the order things apply in.

| | Argo Rollouts | StageSet |
|---|---|---|
| Unit of work | one workload's version + its traffic | a multi-stage release of artifacts |
| Mechanism | weighted traffic shifting + metric analysis | ordered apply with readiness gates + actions |
| Promotion driver | analysis metrics (Prometheus, web, Job) | stage readiness (kstatus, CEL) and actions |
| Pruning / inventory | no (owns the Rollout's pods) | yes (StageInventory record, per-stage prune) |
| GitOps reconcile | via your GitOps tool (e.g. Argo CD) | native (Flux controller) |

## In-app canary vs cross-fleet rollout

Argo Rollouts does canary and blue-green **inside one application** — it shifts
traffic between pod sets, runs analysis, and rolls back the `Deployment`. It does this
maturely, and `StageSet` does not: there is no per-pod traffic shifting here.

A [`FleetRollout`](/gating/fleet-rollout/) works at the other scale — one version
rolled across **many independent StageSets** (one per tenant) in ordered waves, with a
soak, a health gate, and a fleet-wide halt on regression. Argo canaries the pods of a
single `Rollout`; a `FleetRollout` canaries the tenants of a fleet. The two are
complementary axes: run an Argo canary inside each tenant *and* stage the version
across tenants with a `FleetRollout`. On regression a `FleetRollout` can even unwind a
tenant's schema through its
[down migrations](/gating/versioned-migrations/#rolling-back) — a version-level
rollback Argo's manifest-level revert does not reach.

## Using them together

A realistic setup uses both: `StageSet` rolls out the release in order, and a
workload *inside* one stage is itself an Argo `Rollout` doing a canary. `StageSet`
gets the supporting pieces (CRDs, config, migrations) in place and healthy;
`Argo Rollouts` handles the fine-grained traffic progression for that one service.

## Integrating them

Both directions are supported:

- **Argo gating on StageSet.** The controller exports a
  `stageset_stage_ready{namespace,stageset,stage}` gauge that Argo's Prometheus
  metric reads directly, and the stage [gate endpoint](/guides/progressive-delivery/)
  also answers JSON for Argo's web metric. So an Argo `Rollout` can hold its
  promotion until a `StageSet` stage is Ready — no Job bridge needed.
- **StageSet gating on Argo.** A `StageSet` stage's [ready checks](/defining-a-release/ready-checks/)
  can wait (via CEL) on an Argo `Rollout` reaching `Healthy` before the next stage
  runs.

The full, worked examples for both are in the
[progressive-delivery tutorial](/guides/progressive-delivery/#argo-rollouts).
Where the gate's HTTP-status contract is a native fit for
[Flagger](https://flagger.app/), the readiness gauge and JSON endpoint make
`Argo Rollouts` a first-class consumer too.
