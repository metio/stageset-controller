---
title: Install on Kubernetes
description: Prerequisites and the Helm install that get the controller onto a cluster.
tags: [installation, helm, kubernetes]
---

## Prerequisites

- A [Kubernetes](https://kubernetes.io/docs/) cluster with `kubectl` and
  [`helm`](https://helm.sh/) configured against it.
- [Flux](https://fluxcd.io/) `source-controller`, specifically the
  `ExternalArtifact` API (`source.toolkit.fluxcd.io`). A `StageSet` stage always
  resolves to an `ExternalArtifact`, so the CRD must exist. `ExternalArtifact`
  lands in Flux **v2.7.0**; install at least that version. The controller also
  watches `GitRepository`, `OCIRepository`, and `Bucket` sources for
  producer-aware resolution.
- [cert-manager](https://cert-manager.io/), only if you choose the
  `cert-manager` webhook certificate mode. The chart defaults to `self-signed`,
  which provisions and rotates the admission webhook's TLS in-process and needs
  no cert-manager. See [production](/installation/production/#admission-webhook-tls)
  for the trade-off.

[JaaS](https://jaas.projects.metio.wtf/), JOI, or any particular artifact
producer are not required to install the controller — those are sources of
`ExternalArtifact`s, wired up per `StageSet`.

## Install with Helm

The controller is distributed as an OCI [Helm](https://helm.sh/) chart. The
deployment manifests live in the chart, not in the controller repository.

```shell
helm upgrade --install stageset-controller \
  oci://ghcr.io/metio/helm-charts/stageset-controller \
  --namespace stageset-system --create-namespace
```

The container image is `ghcr.io/metio/stageset-controller`; the chart pins the
tag to its own `appVersion` by default.

Every setting referenced across these docs — HA replicas, the rollback store,
webhook mode, NetworkPolicy, the ServiceMonitor, and the rest — is a Helm value.
The [chart's README and `values.yaml`](https://github.com/metio/helm-charts/tree/main/charts/stageset-controller)
document the full, current list.

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

### About the CRDs

The CRDs ship inside the chart's regular templates (not Helm's special `crds/`
directory), so a `helm upgrade` applies schema changes like any other resource.
This is governed by `crds.create` (default `true`). The CRDs carry
`helm.sh/resource-policy: keep`, so a `helm uninstall` leaves them — and your
StageSets — in place; remove them by hand if you really mean to.

If you manage CRDs out of band, the raw definitions are also published in the
controller repository under `config/crd/` and can be applied with
`kubectl apply --server-side -f`.

## Verify

```shell
kubectl -n stageset-system get deploy stageset-controller
kubectl get crd stagesets.stages.metio.wtf stageinventories.stages.metio.wtf
```

Once the controller is `Available`, create your first
[StageSet](/usage/stages-and-sources/).
