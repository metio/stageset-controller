---
title: Stage sources — Git, OCI, Bucket
description: Point a stage straight at a Git/OCI/Bucket source, or render manifests first into an ExternalArtifact.
tags: [tutorials, sources, flux, git]
---

A stage resolves its `sourceRef` to a [Flux](https://fluxcd.io/) artifact. You have
two routes:

```text
manifests in Git / OCI / Bucket  ──────────────────────────►  StageSet   (direct)
manifests in Git / OCI / Bucket  ──►  a renderer (JaaS)  ──►  ExternalArtifact  ──►  StageSet
```

Use the **direct** route when the source already holds ready-to-apply manifests
(the same thing Flux's `kustomize-controller` consumes). Use the **renderer** route
when you generate manifests first — e.g. evaluating Jsonnet with
[JaaS](https://jaas.projects.metio.wtf/).

Copy-pasteable recipes follow, one per source kind. For how `sourceRef`
resolution works as a concept — and the `path`, `prune`, `patches`, and
`postBuild` knobs that shape a stage — see
[stages and sources](/defining-a-release/stages-and-sources/).

## Direct: Git

Point a stage straight at a `GitRepository`:

```yaml
apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: web-manifests
  namespace: apps
spec:
  interval: 1m
  url: https://github.com/acme/web-manifests
  ref:
    branch: main
---
apiVersion: stages.metio.wtf/v1
kind: StageSet
metadata:
  name: web
  namespace: apps
spec:
  stages:
    - name: web
      sourceRef:
        kind: GitRepository
        name: web-manifests
      path: ./manifests        # build a sub-path of the repo
```

## Direct: OCI

Manifests pushed as an OCI artifact (e.g. with `flux push artifact`):

```yaml
apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata:
  name: web-manifests
  namespace: apps
spec:
  interval: 5m
  url: oci://ghcr.io/acme/web-manifests
  ref:
    tag: "2.1.0"
---
apiVersion: stages.metio.wtf/v1
kind: StageSet
metadata:
  name: web
  namespace: apps
spec:
  stages:
    - name: web
      sourceRef:
        kind: OCIRepository
        name: web-manifests
```

## Direct: Bucket

Object storage works the same way:

```yaml
apiVersion: source.toolkit.fluxcd.io/v1
kind: Bucket
metadata:
  name: web-manifests
  namespace: apps
spec:
  interval: 5m
  provider: generic
  bucketName: manifests
  endpoint: minio.storage.svc:9000
  secretRef:
    name: minio-credentials
---
apiVersion: stages.metio.wtf/v1
kind: StageSet
metadata:
  name: web
  namespace: apps
spec:
  stages:
    - name: web
      sourceRef:
        kind: Bucket
        name: web-manifests
```

## Via a renderer (JaaS)

When the source holds *Jsonnet* rather than plain manifests, render it first.
[JaaS](https://jaas.projects.metio.wtf/) reads a Flux source with a `JsonnetSnippet`
and publishes the rendered result as an `ExternalArtifact`, which the stage then
consumes:

```yaml
apiVersion: jaas.metio.wtf/v1
kind: JsonnetSnippet
metadata:
  name: web
  namespace: apps
spec:
  sourceRef:
    kind: GitRepository
    name: web-manifests
    path: ./jsonnet
  entryFile: main.jsonnet
---
apiVersion: stages.metio.wtf/v1
kind: StageSet
metadata:
  name: web
  namespace: apps
spec:
  stages:
    - name: web
      sourceRef:                       # resolve the producer to its ExternalArtifact
        apiVersion: jaas.metio.wtf/v1
        kind: JsonnetSnippet
        name: web
```

Shared libraries arrive the same way:
[JOI](https://github.com/metio/jsonnet-oci-images) publishes Jsonnet libraries as
single-layer OCI images, surfaced as `OCIRepository` + `JsonnetLibrary` pairs a
snippet imports:

```yaml
spec:
  libraries:
    - kind: JsonnetLibrary
      name: k8s-libsonnet
      importPath: k8s          # import 'k8s/...' in your Jsonnet
```

For small or generated snippets, skip the external source and inline the Jsonnet on
the `JsonnetSnippet` (`spec.files`). The end-to-end render-and-roll-out flow is in
[From Jsonnet to a gated rollout](/get-started/jsonnet-to-rollout/).
