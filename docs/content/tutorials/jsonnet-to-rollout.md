---
title: From Jsonnet to a gated rollout
description: Write Jsonnet manifests, render them with JaaS, and roll them out with StageSet — end to end.
tags: [tutorials, jsonnet, jaas, stages]
---

This tutorial follows a complete delivery: write [Kubernetes](https://kubernetes.io/docs/)
manifests in [Jsonnet](https://jsonnet.org/) and publish the source through
[Flux](https://fluxcd.io/); [JaaS](https://jaas.projects.metio.wtf/) renders it into a
Flux `ExternalArtifact`, and a StageSet rolls it out with a readiness gate.

The chain is:

```text
Jsonnet in Git/OCI/Bucket  →  JaaS (JsonnetSnippet)  →  ExternalArtifact  →  StageSet
```

This tutorial renders *Jsonnet*, so it goes through JaaS: JaaS turns the Jsonnet
into an `ExternalArtifact` the stage consumes. (If your manifests were already plain
YAML, a stage could read a `GitRepository`/`OCIRepository`/`Bucket` directly — see
[Stage sources](/tutorials/flux-sources/). The renderer is here because the input is
Jsonnet, not because StageSet can't read Git.)

## Prerequisites

- Flux installed (with the `ExternalArtifact` API — Flux ≥ v2.7.0).
- [JaaS](https://jaas.projects.metio.wtf/) installed in operator mode.
- StageSet installed (see [Installation](/installation/kubernetes/)).
- An `apps` namespace, and a `web-deployer` `ServiceAccount` in it whose RBAC can
  apply the workload (the StageSet applies as it):

  ```shell
  kubectl create namespace apps
  kubectl --namespace apps create serviceaccount web-deployer
  # bind web-deployer to a Role/ClusterRole that can manage Deployments and
  # Services in the apps namespace — see /usage/multi-cluster/ for the tenancy model
  ```

## 1. Write the manifests in Jsonnet

A small web app, parameterized as a Jsonnet top-level function so the same source
renders for any environment. Commit this as `jsonnet/main.jsonnet` in a Git repo:

```jsonnet
// jsonnet/main.jsonnet
function(name='web', image='registry.internal/web:latest', replicas='2') {
  apiVersion: 'v1',
  kind: 'List',
  items: [
    {
      apiVersion: 'apps/v1',
      kind: 'Deployment',
      metadata: { name: name },
      spec: {
        replicas: std.parseInt(replicas),
        selector: { matchLabels: { app: name } },
        template: {
          metadata: { labels: { app: name } },
          spec: { containers: [{ name: name, image: image }] },
        },
      },
    },
    {
      apiVersion: 'v1',
      kind: 'Service',
      metadata: { name: name },
      spec: { selector: { app: name }, ports: [{ port: 80, targetPort: 8080 }] },
    },
  ],
}
```

Rendering a `kind: List` keeps several resources in one document — both the
kustomize build the controller runs and `kubectl` flatten it transparently.

## 2. Publish the source through Flux

Point a Flux `GitRepository` at the repo so the cluster has the Jsonnet:

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
```

Apply it and wait for the source to sync:

```shell
kubectl apply -f gitrepository.yaml
kubectl --namespace apps wait --for=condition=Ready gitrepository/web-manifests
```

## 3. Render with JaaS

A `JsonnetSnippet` reads the Jsonnet from that source, passes the parameters as
top-level arguments, and publishes the rendered result as an `ExternalArtifact`:

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
  tlas:                            # top-level args → the function() parameters
    name: ["web"]
    image: ["registry.internal/web:2.1.0"]
    replicas: ["3"]
```

Apply it; JaaS then publishes an `ExternalArtifact` named `web` in the `apps`
namespace. Confirm it went Ready:

```shell
kubectl apply -f jsonnetsnippet.yaml
kubectl --namespace apps get externalartifact web
```

## 4. Roll it out with StageSet

Reference the `JsonnetSnippet` as the stage source — StageSet resolves the
producer to its `ExternalArtifact` — and gate the stage on the Deployment becoming
available:

```yaml
apiVersion: stages.metio.wtf/v1
kind: StageSet
metadata:
  name: web
  namespace: apps
spec:
  serviceAccountName: web-deployer      # applies run as this tenant SA
  stages:
    - name: web
      sourceRef:
        apiVersion: jaas.metio.wtf/v1
        kind: JsonnetSnippet
        name: web
      readyChecks:
        checks:
          - apiVersion: apps/v1
            kind: Deployment
            name: web
```

Apply it, preview the change before it lands, then watch it roll out:

```shell
kubectl apply -f stageset.yaml
stagesetctl --namespace apps diff web          # preview against live cluster state
stagesetctl --namespace apps get  web          # per-stage progress
```

## 5. Ship a change

Edit `jsonnet/main.jsonnet` (or bump the `image` TLA on the snippet) and commit.
Flux pulls the new commit, JaaS re-renders and republishes the `ExternalArtifact`,
and StageSet — watching the producer — reconciles the new revision through the
same gate. No StageSet edit required.

### No labels or annotations needed

You do **not** annotate or label anything to make this chain fire. The linkage is
the `sourceRef` itself: the controller watches the source *kinds* (`ExternalArtifact`,
`GitRepository`, `OCIRepository`, `Bucket`, and producers like `JsonnetSnippet`) and,
when one changes, maps it back to every StageSet whose `sourceRef` points at it — then
reconciles those. JaaS works the same way for a snippet's own `sourceRef` and
library references. Discovery is automatic; you only declare the references.

## Versioning the rollout

To gate one-time [migrations](/usage/versioned-migrations/) on a release boundary,
declare the version. The simplest is to pin it on the StageSet, bumped alongside the
image:

```yaml
spec:
  version:
    value: "2.1.0"
  migrations:
    - name: backfill-2-0
      to: "2.0.0"               # runs once when the deployed version crosses 2.0.0
      stage: web
      actions:
        - name: backfill
          job:
            sourceRef:
              name: web-migrations
  stages:
    - name: web
      sourceRef:
        apiVersion: jaas.metio.wtf/v1
        kind: JsonnetSnippet
        name: web
```

### Let the version travel with the rendered manifests

Pinning works, but the cleaner pattern is to let the version ride *inside* the
manifests the snippet renders — so a single value flows from your CI all the way to
the rollout gate. Feed the version into the snippet and stamp it onto the standard
`app.kubernetes.io/version` label (and the image tag, from the same value):

```jsonnet
// web.jsonnet
local version = std.extVar('version');   // supplied by JaaS extVars / your CI
{
  apiVersion: 'apps/v1',
  kind: 'Deployment',
  metadata: {
    name: 'web',
    labels: { 'app.kubernetes.io/version': version },   // ← the version, in the manifest
  },
  spec: {
    template: {
      metadata: { labels: { 'app.kubernetes.io/version': version } },
      spec: { containers: [{ name: 'web', image: 'registry.example/web:' + version }] },
    },
  },
}
```

Then point `version.fromObject` at that object and drop the inline `value` — the
controller reads the label off the rendered `Deployment`:

```yaml
spec:
  version:
    fromObject:
      stage: web
      kind: Deployment
      name: web
      # defaults to the app.kubernetes.io/version label
  migrations:
    - name: backfill-2-0
      to: "2.0.0"
      stage: web
      actions:
        - name: backfill
          job:
            sourceRef:
              name: web-migrations
  stages:
    - name: web
      sourceRef:
        apiVersion: jaas.metio.wtf/v1
        kind: JsonnetSnippet
        name: web
```

Now the version has exactly one source of truth — the value your pipeline feeds the
snippet — and it shows up in the image tag, the version label, *and* the migration
gate together. The same `fromObject` works for a `GitRepository`/`OCIRepository`
source too; only a source that ships a dedicated file wants
[`version.fromArtifact`](/usage/versioned-migrations/#from-a-file-in-the-artifact--versionfromartifact)
instead. See [versioned migrations](/usage/versioned-migrations/) for all three.

## Next

From here, add more [stages](/usage/stages-and-sources/), pre/post
[actions](/usage/actions/), or [update windows](/usage/update-windows/) to turn
this single rollout into a gated, multi-stage release. To parameterize per
environment, see [Parameters](/tutorials/parameters/).
