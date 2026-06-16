---
title: Producer-aware sources
description: Reference a producer like JaaS and let the controller find its ExternalArtifact.
tags: [sources, externalartifact, jaas, stages]
---

[Stages and sources](/usage/stages-and-sources/#source-kinds) covers the two
direct routes — an `ExternalArtifact` (the default `sourceRef.kind`) or a Flux
`GitRepository`/`OCIRepository`/`Bucket`. This page covers the third: naming the
thing that *produces* an artifact and letting the controller find it. This is useful
when an operator publishes an `ExternalArtifact` from a custom resource (for example
[JaaS](https://jaas.projects.metio.wtf/) rendering Jsonnet).

## Referencing a producer

Set `kind` (and `apiVersion`) to a producer resource, and the controller resolves
it to the `ExternalArtifact` that producer publishes — the one whose
`spec.sourceRef` back-references the producer (matched on group, kind, and name).
For example, a [JaaS](https://jaas.projects.metio.wtf/) `JsonnetSnippet`
renders Jsonnet and publishes an `ExternalArtifact`; reference the snippet and the
controller follows the link:

```yaml
spec:
  stages:
    - name: dashboards
      sourceRef:
        apiVersion: jaas.metio.wtf/v1
        kind: JsonnetSnippet
        name: grafana-dashboards
```

The controller also watches the common Flux source kinds (`GitRepository`,
`OCIRepository`, `Bucket`) so a stage re-reconciles when an upstream source
changes.

## Related projects

JOI, JaaS, and `StageSet` compose end to end:

- **[JOI](https://github.com/metio/jsonnet-oci-images)** publishes Jsonnet
  libraries as single-layer OCI images (usable both as image-volume mounts and as
  Flux `OCIRepository` sources).
- **[JaaS](https://jaas.projects.metio.wtf/)** evaluates Jsonnet — optionally
  importing those JOI libraries — and publishes the rendered JSON as an
  `ExternalArtifact`.
- **`StageSet`** references the `JsonnetSnippet` (or its artifact) and rolls the
  result out in ordered, gated stages.

Each project is independently useful; a stage reads straight from a
`GitRepository`, `OCIRepository`, or `Bucket`, or from any `ExternalArtifact`
regardless of what produced it.
