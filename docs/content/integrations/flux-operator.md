---
title: Flux Operator (ResourceSet) integration
description: Feed discovered versions into gated promotion, or fan out one StageSet per input.
tags: [flux-operator, resourceset, integration, stages]
---

Pair `StageSet` with the [Flux Operator](https://fluxoperator.dev/)'s
`ResourceSet` to combine *discovery* and *fan-out* with *ordered, gated
promotion*. `ResourceSet` and `ResourceSetInputProvider` (RSIP) discover and
template inputs; `StageSet` rolls a single app through its stages. The two work on
[complementary axes](/comparisons/flux-operator/) — this page shows the two
patterns where they meet.

Both patterns require the
[Flux Operator installed](https://fluxoperator.dev/docs/install/) alongside
Flux and the stageset-controller.

## Discovery feeding gated promotion

A `ResourceSetInputProvider` watches an OCI registry for new chart or image tags
on a schedule, and a `ResourceSet` templates a `StageSet` pinned to whatever tag
it discovers. The `StageSet` then promotes that version through its ordered,
gated stages.

The RSIP discovers tags, filtered to a semver range, refreshing only inside a
weekday morning window:

```yaml
apiVersion: fluxcd.controlplane.io/v1
kind: ResourceSetInputProvider
metadata:
  name: my-app-tags
  namespace: apps
spec:
  type: OCIArtifactTag
  url: oci://ghcr.io/example/my-app
  filter:
    semver: ">=1.0.0 <2.0.0"
    limit: 1
  schedule:
    - cron: "0 8 * * 1-5"
      timeZone: Europe/Berlin
      window: 1h
```

The `ResourceSet` consumes that provider's exported inputs and renders a
`StageSet` pinned to the discovered `<< inputs.tag >>`:

```yaml
apiVersion: fluxcd.controlplane.io/v1
kind: ResourceSet
metadata:
  name: my-app-stageset
  namespace: apps
spec:
  serviceAccountName: my-app-deployer
  inputsFrom:
    - kind: ResourceSetInputProvider
      name: my-app-tags
  resources:
    - apiVersion: source.toolkit.fluxcd.io/v1
      kind: OCIRepository
      metadata:
        name: my-app
        namespace: apps
      spec:
        interval: 10m
        url: oci://ghcr.io/example/my-app
        ref:
          tag: << inputs.tag | quote >>
    - apiVersion: stages.metio.wtf/v1
      kind: StageSet
      metadata:
        name: my-app
        namespace: apps
      spec:
        serviceAccountName: my-app-deployer
        stages:
          - name: crds
            sourceRef:
              apiVersion: source.toolkit.fluxcd.io/v1
              kind: OCIRepository
              name: my-app
            path: ./crds
          - name: app
            sourceRef:
              apiVersion: source.toolkit.fluxcd.io/v1
              kind: OCIRepository
              name: my-app
            path: ./app
            readyChecks:
              checks:
                - apiVersion: apps/v1
                  kind: Deployment
                  name: my-app
                  namespace: apps
```

The RSIP's `schedule.window` already constrains when a new tag becomes visible, so
the `StageSet` here carries no `updateWindows` — gating lives at the discovery
layer. A new tag inside the morning window re-renders the `OCIRepository` and
`StageSet`, and the promotion runs CRDs before the app, waiting on the
`Deployment`'s health between stages.

## Fanning out one StageSet per input

A single `ResourceSet` can stamp out one `StageSet` per input — per tenant,
branch, or pull request — each promoted through the same stages independently.
Here a `Static` provider lists the tenants:

```yaml
apiVersion: fluxcd.controlplane.io/v1
kind: ResourceSetInputProvider
metadata:
  name: tenants
  namespace: apps
spec:
  type: Static
  defaultValues:
    region: eu-central-1
```

```yaml
apiVersion: fluxcd.controlplane.io/v1
kind: ResourceSet
metadata:
  name: tenant-stagesets
  namespace: apps
spec:
  inputs:
    - tenant: acme
    - tenant: globex
    - tenant: initech
  resources:
    - apiVersion: stages.metio.wtf/v1
      kind: StageSet
      metadata:
        name: << inputs.tenant >>-app
        namespace: apps
      spec:
        serviceAccountName: << inputs.tenant >>-deployer
        stages:
          - name: app
            sourceRef:
              name: << inputs.tenant >>-app   # an ExternalArtifact per tenant
        updateWindows:
          - type: Allow
            schedule: "0 14 * * TUE,THU"
            duration: 3h
            timeZone: Europe/Berlin
```

Each rendered `StageSet` impersonates its own tenant ServiceAccount, so a tenant's
rollout can only touch what that account allows. Adding a tenant is one more entry
under `spec.inputs`; the `ResourceSet` reconciles the new `StageSet` into being.

## Choosing where to time-gate

Both tools can gate on a clock — the RSIP `spec.schedule` constrains *when inputs
refresh*, while a `StageSet`'s [update windows](/gating/update-windows/) constrain
*when a revision rolls out*. Pick one layer per release so a rollout isn't gated
twice:

- Gate at the **RSIP** when you want discovery itself to be periodic — a new
  version only ever appears during the schedule window (the first pattern above).
- Gate at the **StageSet** when inputs may refresh freely but promotion must wait
  for a maintenance window (the second pattern above).

See the [comparison](/comparisons/flux-operator/) for how the two axes line up.
