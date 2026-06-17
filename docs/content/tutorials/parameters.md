---
title: Parameterizing a rollout
description: Two layers of parameters — JaaS TLAs/extVars at render time, StageSet postBuild substitution at delivery.
tags: [tutorials, tlas, ext-vars, jaas]
---

A rollout takes parameters at two distinct layers, which serve different purposes:

- **Render-time parameters (JaaS).** Change *what gets rendered*. The Jsonnet
  computes its output from top-level arguments (`tlas`) and external variables
  (`externalVariables`). Different values produce a different `ExternalArtifact`.
- **Delivery-time parameters (StageSet `postBuild`).** Inject values *into
  already-rendered manifests*, per stage, by string substitution — the same
  mechanism Flux's `kustomize-controller` uses.

Use render-time parameters for structural logic; use delivery-time parameters to
stamp environment-specific values onto a shared artifact.

## Render-time: JaaS TLAs and external variables

Top-level arguments map to a Jsonnet `function(...)`:

```jsonnet
// main.jsonnet
function(name='web', replicas='2')
  { apiVersion: 'apps/v1', kind: 'Deployment', metadata: { name: name },
    spec: { replicas: std.parseInt(replicas) /* … */ } }
```

```yaml
apiVersion: jaas.metio.wtf/v1
kind: JsonnetSnippet
metadata:
  name: web
  namespace: apps
spec:
  sourceRef: { kind: GitRepository, name: web-manifests, path: ./jsonnet }
  tlas:                          # → function(name, replicas)
    name: ["web"]
    replicas: ["3"]
  externalVariables:            # → std.extVar('environment')
    environment: "production"
```

`tlas` is a map of name → list of values (a single-element list for a scalar
argument; multiple values become a JSON array). `externalVariables` are plain
strings read with `std.extVar`.

## Delivery-time: StageSet postBuild substitution

When the rendered manifests carry `${var}` placeholders, a stage substitutes them
at apply time — from inline values, ConfigMaps, and Secrets:

```yaml
apiVersion: stages.metio.wtf/v1
kind: StageSet
metadata:
  name: web
  namespace: apps
spec:
  stages:
    - name: web
      sourceRef:
        apiVersion: jaas.metio.wtf/v1
        kind: JsonnetSnippet
        name: web
      postBuild:
        substitute:
          cluster_name: prod-eu
        substituteFrom:
          - kind: ConfigMap
            name: cluster-vars
          - kind: Secret
            name: cluster-secrets
            optional: true
```

A manifest field like `value: "${cluster_name}"` becomes `value: "prod-eu"` for
this stage.

> **A substituted number is a number.** Substitution is textual and runs after
> kustomize, which drops the quotes around a plain `${var}` scalar — so a numeric
> value such as `replicas: "${COUNT}"` with `COUNT: "3"` lands as the YAML
> integer `3`, not the string `"3"`. For fields that must be strings —
> `ConfigMap`/`Secret` `data`, label and annotation values — keep the substituted
> value non-numeric (e.g. `"v${COUNT}"`) or build it with a `configMapGenerator`,
> otherwise the apply fails with `expected string, got int`. This matches Flux
> kustomize-controller's post-build substitution.

## Reusing one artifact across environments

The two layers combine into a common pattern: render an environment-*agnostic*
artifact once with JaaS, then have several StageSets — one per environment —
consume that same artifact and stamp their own values with `postBuild`:

```yaml
# staging
spec:
  stages:
    - name: web
      sourceRef: { apiVersion: jaas.metio.wtf/v1, kind: JsonnetSnippet, name: web }
      postBuild:
        substituteFrom:
          - { kind: ConfigMap, name: staging-vars }
---
# production (same artifact, different values)
spec:
  stages:
    - name: web
      sourceRef: { apiVersion: jaas.metio.wtf/v1, kind: JsonnetSnippet, name: web }
      postBuild:
        substituteFrom:
          - { kind: ConfigMap, name: production-vars }
```

One render, many environments — each StageSet bounded by its own
[ServiceAccount](/usage/multi-cluster/) and gated by its own
[actions](/usage/actions/) and [update windows](/usage/update-windows/).
