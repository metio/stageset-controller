---
title: Generating migrations with Jsonnet
description: Render a version-gated migration ladder with JaaS and consume it from a StageSet via migrationsSourceRef.
tags: [tutorials, migrations, jsonnet, jaas, sources]
---

A [migration ladder](/usage/versioned-migrations/) is just data — a list of
version-gated actions. So you can *generate* it with [Jsonnet](https://jsonnet.org/),
publish it once as a Flux artifact with [JaaS](https://jaas.projects.metio.wtf/),
and have every StageSet that deploys the app consume it through
`spec.migrationsSourceRef`. One ladder, authored once, shared across namespaces.

The chain mirrors [From Jsonnet to a gated rollout](/tutorials/jsonnet-to-rollout/),
but the rendered artifact is the *migrations* rather than the manifests:

```text
Jsonnet ladder  →  JaaS (JsonnetSnippet)  →  ExternalArtifact  →  StageSet.migrationsSourceRef
```

## Prerequisites

- Flux installed (with the `ExternalArtifact` API — Flux ≥ v2.7.0).
- [JaaS](https://jaas.projects.metio.wtf/) installed in operator mode.
- StageSet installed (see [Installation](/installation/kubernetes/)).

## 1. Write the ladder in Jsonnet

The snippet evaluates to a **JSON array of migrations** — the same shape the
ladder file uses, so JaaS's `rendered.json` *is* the ladder. A small helper keeps
each entry readable, and the file is trivially parameterizable (per app, per
environment) with top-level args:

```jsonnet
// migrations/main.jsonnet
local del(name, kind, obj) = {
  name: name,
  delete: { target: { apiVersion: 'v1', kind: kind, name: obj } },
};
[
  {
    name: 'drop-legacy-config',
    to: '2.0.0',
    from: '>=1.0.0',            // explicit operator — a bare "1.0.0" is rejected
    stage: 'db-pre',           // anchors by role; consuming stages declare it
    actions: [del('drop-legacy', 'ConfigMap', 'legacy-config')],
  },
]
```

Commit it to a Git repo (or push it to an OCI/Bucket source) JaaS can read.
Validate it before publishing with the controller's own checks:

```shell
stagesetctl lint-migrations migrations/  # if you render locally; see lint-migrations
```

## 2. Render it with JaaS

A `JsonnetSnippet` points JaaS at the source; JaaS publishes an `ExternalArtifact`
of the same name carrying `rendered.json`:

```yaml
apiVersion: jaas.metio.wtf/v1
kind: JsonnetSnippet
metadata:
  name: orders-migrations
  namespace: apps
spec:
  sourceRef:
    kind: GitRepository
    name: orders-migrations-src
    path: ./migrations
  entryFile: main.jsonnet
```

```shell
kubectl apply -f jsonnetsnippet.yaml
kubectl --namespace apps get externalartifact orders-migrations
```

## 3. Consume it from a StageSet

Reference the `JsonnetSnippet` as the migrations source — StageSet resolves the
producer to its `ExternalArtifact` — and declare the `db-pre` anchor role on the
stage the ladder anchors to:

```yaml
apiVersion: stages.metio.wtf/v1
kind: StageSet
metadata:
  name: orders
  namespace: apps
spec:
  serviceAccountName: orders-deployer
  version:
    fromObject:
      stage: app
      kind: Deployment
      name: orders
  migrationsSourceRef:
    sourceRef:
      kind: JsonnetSnippet
      name: orders-migrations
  stages:
    - name: app
      migrationAnchor: db-pre        # the ladder's stage: db-pre resolves here
      sourceRef:
        kind: JsonnetSnippet
        name: orders
```

When `orders` crosses into `2.0.0`, the rendered `drop-legacy-config` migration
runs before the `app` stage. Every other namespace deploying orders references the
**same** `orders-migrations` snippet output — the ladder lives once, in Jsonnet.

## Production notes

- **Validate in CI** with [`stagesetctl lint-migrations`](/cli/lint-migrations/)
  on the Jsonnet output before it's published, so authoring errors never reach a
  cluster.
- **Verify and pin** the source in production — see
  [Verifying and pinning the source](/usage/versioned-migrations/#verifying-and-pinning-the-source).
  An OCI-published rendered ladder can be cosign-signed and digest-pinned.
- **Approve** destructive transitions with `spec.version.requireApproval` if you
  want a human in the loop before they run.
