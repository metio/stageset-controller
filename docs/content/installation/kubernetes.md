---
title: Install on Kubernetes
description: Prerequisites, the idempotent Helm deploy, how upgrades treat the CRDs, where to customize, and how to verify the controller is healthy.
tags: [installation, helm, kubernetes]
---

The controller is distributed as a container image at
`ghcr.io/metio/stageset-controller` and as an OCI [Helm](https://helm.sh/) chart
at `oci://ghcr.io/metio/helm-charts/stageset-controller`. The deployment
manifests live in the chart, not in the controller repository.

## Prerequisites

- A [Kubernetes](https://kubernetes.io/) cluster with `kubectl` and
  [`helm`](https://helm.sh/) **v3.14 or later** (OCI chart support) configured
  against it.
- [Flux](https://fluxcd.io/) `source-controller`, specifically the
  `ExternalArtifact` API (`source.toolkit.fluxcd.io`). A `StageSet` stage always
  resolves to an `ExternalArtifact`, so the CRD must exist. `ExternalArtifact`
  lands in Flux **v2.7.0**; install at least that version. The controller also
  watches `GitRepository`, `OCIRepository`, and `Bucket` sources for
  producer-aware resolution.
- [cert-manager](https://cert-manager.io/) — **only** if you choose the
  `cert-manager` webhook certificate mode. The chart defaults to `self-signed`,
  which provisions and rotates the admission webhook's TLS in-process and needs
  no cert-manager. See [Production](/installation/production/#admission-webhook-tls)
  for the trade-off.

[JaaS](https://jaas.projects.metio.wtf/), JOI, or any particular artifact
producer are **not** required to install the controller — those are sources of
`ExternalArtifact`s, wired up per `StageSet`.

## Install and update

`helm upgrade --install` is idempotent: the same command installs the chart the
first time and applies your changes on every subsequent run, so it's the only
deploy command you need. To update later, re-run it with an updated `--values`
file or `--set` flags.

```shell
helm upgrade --install stageset-controller \
  oci://ghcr.io/metio/helm-charts/stageset-controller \
  --namespace stageset-system --create-namespace \
  --values my-values.yaml \
  --wait
```

The chart pins the image tag to its own `appVersion` by default.

### What the chart installs

- The **controller `Deployment`**, its `ServiceAccount`, and the cluster RBAC it
  needs (a `ClusterRole` + `ClusterRoleBinding`, plus a namespaced leader-election
  `Role`/`RoleBinding`).
- The **CRDs** — `StageSet` and `StageInventory`.
- The **validating admission webhook** (`ValidatingWebhookConfiguration` + a
  webhook `Service`).
- A **metrics `Service`** (and an opt-in `ServiceMonitor`).
- The **Flagger gate `Service`** for the read-only stage-gate endpoint.
- Opt-in extras: `NetworkPolicy`, `PodDisruptionBudget`,
  `HorizontalPodAutoscaler`, a rollback-store `PersistentVolumeClaim`, and a
  managed `Namespace`.

### How CRDs are handled

The CRDs ship inside the chart's regular templates (not Helm's special `crds/`
directory), so a `helm upgrade --install` applies schema changes like any other
resource. This is governed by `crds.create` (default `true`). The CRDs carry
`helm.sh/resource-policy: keep`, so a `helm uninstall` leaves them — and your
StageSets — in place; remove them by hand only if you really mean to.

If you manage CRDs out of band, the raw definitions are also published in the
controller repository under `config/crd/` and can be applied with
`kubectl apply --server-side -f`.

## Customize

Every setting referenced across these docs — HA replicas, the rollback store,
webhook mode, NetworkPolicy, service mesh, the ServiceMonitor, and the rest — is
a Helm value. Two references cover them:

- [Helm chart values](/installation/helm-values/) — the full values reference,
  generated from the chart's own schema.
- [Configuration reference](/installation/configuration/) — every controller flag
  and the chart value that drives it.

For production sizing — the rollback store, multi-replica HA, observability, and
webhook hardening — see the [Production guide](/installation/production/).

## Verify

```shell
kubectl --namespace stageset-system rollout status deploy/stageset-controller
kubectl get crd stagesets.stages.metio.wtf stageinventories.stages.metio.wtf
```

Once the controller is `Available`, create your first
[StageSet](/usage/stages-and-sources/).
