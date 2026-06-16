---
title: Quickstart
description: Install the controller and roll out your first StageSet in a few steps.
tags: [tutorials, quickstart, getting-started]
---

This tutorial takes you from an empty cluster to one running StageSet. The path
is the shortest one — a single stage pointing directly at a Flux
`GitRepository` that already holds plain manifests. No Jsonnet, no migrations,
no optional knobs.

## Prerequisites

- A [Kubernetes](https://kubernetes.io/docs/) cluster with `kubectl` configured
  against it.
- `helm` 3.x.
- [Flux](https://fluxcd.io/) **v2.7.0 or newer** — the `ExternalArtifact` CRD a
  stage resolves to lands in that version. See
  [Install on Kubernetes](/installation/kubernetes/#prerequisites) for the full
  prerequisites.

## Step 1 — Install the controller

```shell
helm upgrade --install stageset-controller \
  oci://ghcr.io/metio/helm-charts/stageset-controller \
  --namespace stageset-system --create-namespace \
  --wait --timeout 5m
```

See [Install on Kubernetes](/installation/kubernetes/) for the full list of chart
values (HA replicas, rollback store, webhook TLS mode, and so on).

Verify the controller is running:

```shell
kubectl -n stageset-system get deploy stageset-controller
# NAME                   READY   UP-TO-DATE   AVAILABLE   AGE
# stageset-controller    1/1     1            1           30s
```

## Step 2 — Provide a source

A stage reads from a Flux source. The quickest path is a `GitRepository`
pointing at a repo that contains plain Kubernetes manifests:

```shell
cat <<EOF | kubectl apply -f -
apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: my-app
  namespace: default
spec:
  interval: 1m
  url: https://github.com/acme/my-app-manifests
  ref:
    branch: main
EOF
```

Wait for it to sync:

```shell
kubectl -n default wait --for=condition=Ready gitrepository/my-app --timeout=2m
```

## Step 3 — Apply a StageSet

Only `spec.stages` is required. Each stage needs a `name` and a `sourceRef`.
`sourceRef.kind` defaults to `ExternalArtifact`; for a `GitRepository` name it
explicitly:

```shell
cat <<EOF | kubectl apply -f -
apiVersion: stages.metio.wtf/v1
kind: StageSet
metadata:
  name: my-app
  namespace: default
spec:
  stages:
    - name: app
      sourceRef:
        kind: GitRepository
        name: my-app
EOF
```

## Step 4 — Confirm it reconciled

```shell
kubectl -n default get stageset my-app
# NAME     READY   AGE
# my-app   True    15s
```

If `READY` is `False`, describe the resource — the `Reason` and `Message` on the
`Ready` condition identify the problem:

```shell
kubectl -n default describe stageset my-app
```

For a richer view of per-stage progress, use the CLI:

```shell
stagesetctl get my-app -n default
```

See [CLI reference](/cli/) for all `stagesetctl` commands.

## Where to go next

- [From Jsonnet to a gated rollout](/tutorials/jsonnet-to-rollout/) — the
  flagship tutorial: render Jsonnet with [JaaS](https://jaas.projects.metio.wtf/),
  gate with readiness checks, add versioned migrations.
- [Stage sources](/tutorials/flux-sources/) — direct `GitRepository`,
  `OCIRepository`, and `Bucket` sources, plus the renderer route.
- [Usage](/usage/) — every configuration knob, one page per concern.
- [Installation](/installation/) — production-grade install: HA, rollback store,
  webhook TLS, NetworkPolicy.
