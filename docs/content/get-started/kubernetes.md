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
  no cert-manager. See [Production](/running/production/#admission-webhook-tls)
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

The chart pins the image tag to its own `appVersion` by default. The chart is
also listed on [ArtifactHub](https://artifacthub.io/packages/helm/stageset-controller/stageset-controller),
where you can browse its versions, values, and changelog.

### Install with Flux (GitOps)

To manage the controller from a Flux GitOps repository instead of `helm` on the
command line, point an `OCIRepository` at the chart and install it with a
`HelmRelease`:

```yaml
apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata:
  name: stageset-controller
  namespace: stageset-system
spec:
  interval: 1h
  url: oci://ghcr.io/metio/helm-charts/stageset-controller
  ref:
    # Latest released chart; pin to a tag for production (Renovate can bump it).
    semver: ">=0.0.0"
---
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: stageset-controller
  namespace: stageset-system
spec:
  interval: 1h
  chartRef:
    kind: OCIRepository
    name: stageset-controller
```

The chart's defaults are a sensible single-replica install, so this needs no
`values:` block; add one (the same keys as the `--values` file) to enable HA, the
rollback-store PVC, NetworkPolicy, and the rest — see the
[values reference](/reference/helm-values/). Both resources live in
`stageset-system`, so create that namespace first
(`kubectl create namespace stageset-system`, or include a `Namespace` manifest
alongside them). The source- and helm-controllers reconcile an `OCIRepository`
and `HelmRelease` directly, so no wrapping `Kustomization` is required — apply the
two with `kubectl apply -f`, or commit them to whatever source your Flux setup
already syncs. `HelmRelease` gives you upgrades, rollbacks, and drift correction;
pull the chart version forward by bumping the `OCIRepository` `ref`.

### What the chart installs

- The **controller `Deployment`**, its `ServiceAccount`, and the cluster RBAC it
  needs (a `ClusterRole` + `ClusterRoleBinding`, plus a namespaced leader-election
  `Role`/`RoleBinding`).
- The **CRDs** — `StageSet` and `StageInventory`.
- The **validating admission webhook** (`ValidatingWebhookConfiguration` + a
  webhook `Service`).
- A **metrics `Service`** (and an opt-in `ServiceMonitor`).
- The **Flagger gate `Service`** for the read-only gate endpoint.
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

- [Helm chart values](/reference/helm-values/) — the full values reference,
  generated from the chart's own schema.
- [Configuration reference](/reference/configuration/) — every controller flag
  and the chart value that drives it.

For production sizing — the rollback store, multi-replica HA, observability, and
webhook hardening — see the [Production guide](/running/production/).

## Verify

```shell
kubectl --namespace stageset-system rollout status deploy/stageset-controller
kubectl get crd stagesets.stages.metio.wtf stageinventories.stages.metio.wtf
```

Once the controller is `Available`, create your first
[StageSet](/defining-a-release/stages-and-sources/).
