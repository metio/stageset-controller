---
title: Stages and sources
description: The core — ordered stages applying Flux sources, with path, prune, patches, and substitution.
tags: [stages, sources, flux, externalartifact]
---

A `StageSet` is an ordered list of stages. Each stage resolves a
[Flux](https://fluxcd.io/) source — a `GitRepository`, `OCIRepository`, `Bucket`,
or an `ExternalArtifact` (the default) — applies its manifests, waits for them to
become healthy, and only then lets the next stage start.

## One stage

The minimum is one stage pointing at one artifact in the same namespace:

```yaml
apiVersion: stages.metio.wtf/v1
kind: StageSet
metadata:
  name: my-app
  namespace: default
spec:
  stages:
    - name: app
      sourceRef:
        name: my-app          # an ExternalArtifact
```

`sourceRef.kind` defaults to `ExternalArtifact`, so the common case is a single
line. The controller fetches the artifact, applies every manifest in it, and marks
the stage `Ready` once the applied objects report healthy.

## Source kinds

A `sourceRef` resolves to a Flux artifact three ways. Point it at whichever you
already have:

```yaml
# 1. an ExternalArtifact (the default — kind omitted)
sourceRef:
  name: my-app

# 2. a classic Flux source, consumed directly
sourceRef:
  kind: GitRepository        # or OCIRepository, or Bucket
  name: my-app-manifests

# 3. a producer that publishes an ExternalArtifact (resolved via its back-pointer)
sourceRef:
  apiVersion: jaas.metio.wtf/v1
  kind: JsonnetSnippet
  name: my-app
```

`GitRepository`, `OCIRepository`, and `Bucket` carry the same `status.artifact`
contract as `ExternalArtifact`, so the controller reads them directly — no producer
in between. A stage can apply manifests straight from a Git repo or an OCI artifact,
like Flux's own `kustomize-controller`. For the producer case (for example
rendering Jsonnet with [JaaS](https://jaas.projects.metio.wtf/)), see
[producer-aware sources](/integrations/producer-aware-sources/).

## Ordered stages

Add more stages and they run top to bottom — each one waits for the previous to be
`Ready`:

```yaml
spec:
  stages:
    - name: crds          # 1 ── install the CRDs first
      sourceRef:
        name: platform-crds
    - name: operator      # 2 ── then the operator that needs them
      sourceRef:
        name: platform-operator
    - name: workloads     # 3 ── then the workloads it manages
      sourceRef:
        name: team-workloads
```

This is the core of a `StageSet`: `operator` is never applied until `crds` is
healthy, so the operator never crash-loops waiting for a CRD that isn't there yet.

## Shaping a stage's manifests

A stage can build from a sub-path of the artifact, customize with patches, and
substitute variables — the [kustomize](https://kubectl.docs.kubernetes.io/)-style
surface:

```yaml
spec:
  stages:
    - name: app
      sourceRef:
        name: my-app
      path: ./overlays/production      # build a sub-path of the artifact
      prune: true                      # GC objects that leave this stage (default)
      patches:
        - patch: |
            - op: replace
              path: /spec/replicas
              value: 6
          target:
            kind: Deployment
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

- **`path`** builds from a directory inside the artifact (default `./`).
- **`prune`** (default `true`) garbage-collects objects that fall out of the stage
  between reconciles, tracked precisely via the stage's
  [`StageInventory`](/api/stageinventory/).
- **`patches`** are strategic-merge or JSON6902 patches applied after the build.
- **`postBuild`** substitutes `${var}` references from inline values, ConfigMaps,
  and Secrets at delivery time — see [parameterizing a rollout](/guides/parameters/)
  for the full render-time-vs-delivery-time treatment.

## An artifact with no manifests fails the stage

A stage whose artifact carries no `.yaml`, `.yml`, or `.json` file under its
`path` fails immediately, naming the path rather than letting the stage apply
nothing and report success. The usual causes are a `path` that doesn't match the
artifact's layout, a source publishing something other than manifests, or ignore
rules that pruned more than intended:

```yaml
# A GitRepository that publishes an empty artifact: /* excludes everything,
# and the re-include never matches because its parent directory is excluded.
ignore: |
  /*
  !/releases/app-1.30.0.yaml
```

A render that legitimately produces **zero objects** is different and still
allowed — a JsonnetSnippet emitting `[]` for a disabled feature publishes a
`rendered.json`, so its artifact has a manifest file and simply builds nothing.

To remove a stage's objects, delete the stage from `spec.stages` (or the whole
StageSet): its objects are torn down in reverse recorded order. Emptying the
artifact is not a way to ask for that — the spec would still be asking for a
deployment, and a mistaken ignore rule would be indistinguishable from the
request.

From here, layer on [actions](/defining-a-release/actions/) to gate the stage, or
[ready checks](/defining-a-release/ready-checks/) to define what "healthy" means.
