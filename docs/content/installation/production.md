---
title: Production
description: High availability, security hardening, and on-prem and EKS HA reference setups.
tags: [production, security, operations]
---

## High availability

The controller supports leader-elected HA. Enable leader election and run more
than one replica; only the lease holder reconciles, while every replica answers
admission webhook calls (admission must stay available even on non-leaders).

- Leader election is toggled with `--leader-elect`. The binary defaults it to
  `false`, but the **Helm chart enables it by default** (`controller.leaderElect:
  true`), so a default install is already lease-guarded even at one replica.
- The lease is named `stageset-controller.stages.metio.wtf` and lives in the
  controller's namespace. It uses controller-runtime's default timing (~15 s
  lease duration). The lease is **not** released eagerly on shutdown, so after a
  rolling update the new leader takes over when the old lease expires — budget a
  few seconds of reconcile pause on restart (admission and the gate endpoint are
  unaffected).
- Scaling: when the chart's `replicas.max` exceeds `replicas.min` it renders a
  `HorizontalPodAutoscaler` (CPU target 80%) and a `PodDisruptionBudget`
  (`minAvailable: 1`). At the default 1/1 it sets neither and leaves
  `spec.replicas` unmanaged.

The controller watches every namespace by default. Multi-tenancy is enforced per
`StageSet` through impersonation (see below). You can additionally scope the
controller to a namespace set with `controller.watchNamespaces` — one controller
instance per tenant-group — and run it under `cluster-admin` for single-tenant
clusters; both are covered in
[multi-cluster and tenancy](/usage/multi-cluster/).

## Hardening

Each option below is shown as the Helm values that configure it. Several are
already the chart's defaults, shown so you can see what is applied and override
it for a stricter policy.

### Tenant impersonation

The controller never applies your manifests with its own identity. Every cluster
operation for a `StageSet` — building, applying, pruning, running actions — is
performed impersonating the `StageSet`'s `spec.serviceAccountName` (the chart
grants the controller `impersonate`, not write access). A `StageSet` can only do
what its tenant SA permits; an over-broad or missing SA fails closed.

This one lives on the `StageSet`, not in the chart — give every production
`StageSet` a scoped `ServiceAccount`:

```yaml
apiVersion: stages.metio.wtf/v1
kind: StageSet
metadata: { name: payments, namespace: payments }
spec:
  serviceAccountName: payments-deployer   # scoped to exactly this release's needs
  # …
```

### Pod security context

The chart runs a non-root, read-only-root-filesystem pod with all capabilities
dropped, on a `gcr.io/distroless/static:nonroot` image (no shell or package
manager). These are the rendered defaults:

```yaml
podSecurityContext:
  runAsNonRoot: true
  seccompProfile:
    type: RuntimeDefault
securityContext:
  runAsNonRoot: true
  runAsUser: 65532
  runAsGroup: 65532
  allowPrivilegeEscalation: false
  readOnlyRootFilesystem: true
  capabilities:
    drop: [ALL]
  seccompProfile:
    type: RuntimeDefault
```

### Resource limits

Requests equal limits, so the pod is fully constrained:

```yaml
resources:
  cpu: 50m
  memory: 256Mi
  ephemeralStorage: 32Mi   # /tmp and the self-signed cert dir are emptyDirs
```

### Pod-Security Standards namespace

Have the chart create the install namespace with restricted PSS labels:

```yaml
namespace:
  create: true
  pssLevel: restricted     # or: baseline / privileged
```

### Network policy

The gate endpoint is **unauthenticated** (read-only
`GET /gate/{namespace}/{stageset}/{stage}`). Turn on the ingress-only NetworkPolicy
to fence it — and the webhook/metrics ports — to only the peers that need them:

```yaml
networkPolicy:
  enabled: true            # admits the webhook (9443), metrics (8080), gate (8082)
```

The policy is **ingress-only**, so it does not restrict egress — the controller can
still fetch stage artifacts over HTTP from source-controller (an `ExternalArtifact`
or a `GitRepository`/`OCIRepository`/`Bucket` is served from the same artifact
endpoint). If your cluster default-denies egress, add an egress allowance to
source-controller (and DNS) so those fetches succeed.

### Admission webhook TLS

`webhook.certMode` chooses how the webhook serving certificate is obtained:

```yaml
webhook:
  certMode: cert-manager   # cert-manager issues + rotates the cert (requires cert-manager)
  # certMode: self-signed  # chart default: in-pod CA + serving cert, rotated at
  #                          validity/3, with no cert-manager dependency
```

## Reference setups

Two HA shapes — on-prem with shared RWX storage, and AWS/EKS with S3 — over the
same backbone: a leader-elected pair (or trio), a rollback store reachable from
whichever pod holds the lease, cert-manager for the webhook, a `NetworkPolicy`
fencing the unauthenticated gate, and a `ServiceMonitor` if you run Prometheus.

Both run two replicas for [HA](#high-availability) (`replicas.max` above
`replicas.min` also renders a PDB and an HPA) and set
`webhook.certMode: cert-manager`, so [cert-manager](https://cert-manager.io/) must
be installed in the cluster.

### On-prem (RWX storage)

The rollback store gives bit-exact rollbacks that outlive producer GC. With HA
replicas it must be reachable from whichever pod holds the lease, so use a
`ReadWriteMany` PVC on your on-prem storage class — every replica mounts the same
volume.

```yaml
# values-onprem.yaml
replicas:
  min: 2                 # leader-elected HA; the non-leader still serves admission
  max: 3                 # > min renders an HPA (CPU 80%) and a PodDisruptionBudget

controller:
  leaderElect: true

rollbackStore:
  backend: pvc
  pvc:
    accessModes: [ReadWriteMany]
    storageClass: nfs-client     # your RWX class (NFS, CephFS, …)
    size: 10Gi

webhook:
  certMode: cert-manager         # requires cert-manager in the cluster

networkPolicy:
  enabled: true                  # fences the unauthenticated gate endpoint

metrics:
  serviceMonitor:
    enabled: true
```

```shell
helm upgrade --install stageset-controller \
  oci://ghcr.io/metio/helm-charts/stageset-controller \
  --namespace stageset-system --create-namespace \
  -f values-onprem.yaml
```

### AWS / EKS (S3)

On EKS, back the rollback store with S3 and let the controller assume an IAM role
through [IRSA](https://docs.aws.amazon.com/eks/latest/userguide/iam-roles-for-service-accounts.html)
— no static keys. Annotate the controller's ServiceAccount with the role ARN and
leave the S3 credentials empty; the store's minio-go client picks the role up from
the pod's web-identity token.

```yaml
# values-eks.yaml
replicas:
  min: 2
  max: 3

controller:
  leaderElect: true

serviceAccount:
  annotations:
    # an IAM role granting s3:GetObject/PutObject/ListBucket/DeleteObject on the bucket
    eks.amazonaws.com/role-arn: arn:aws:iam::123456789012:role/stageset-controller

rollbackStore:
  backend: s3
  s3:
    endpoint: s3.eu-west-1.amazonaws.com
    bucket: my-org-stageset-rollback
    region: eu-west-1
    # no existingSecret → credentials come from the IRSA role above

webhook:
  certMode: cert-manager

networkPolicy:
  enabled: true

metrics:
  serviceMonitor:
    enabled: true
```

```shell
helm upgrade --install stageset-controller \
  oci://ghcr.io/metio/helm-charts/stageset-controller \
  --namespace stageset-system --create-namespace \
  -f values-eks.yaml
```

### Alongside the other Flux controllers

`stageset-controller` is a [Flux](https://fluxcd.io/) citizen and needs no special
wiring to coexist with `source-controller`, `kustomize-controller`,
`helm-controller`, and `notification-controller`. It reads `ExternalArtifact` (and
the standard `GitRepository`, `OCIRepository`, and `Bucket` sources) from
`source-controller`, and `notification-controller` routes its events through an
`Alert` that targets `kind: StageSet` — no Provider/Alert plumbing of its own.
Install it in its own namespace (e.g. `stageset-system`) next to `flux-system`;
the only cluster-scoped pieces are its CRDs, `ClusterRole`, and webhook
configuration.

### Alongside JaaS

[JaaS](https://jaas.projects.metio.wtf/) renders Jsonnet and publishes the result
as an `ExternalArtifact`, which is what a `StageSet` stage consumes — so the two
compose directly. Reference the artifact by name, or name the producing
`JsonnetSnippet` and let `stageset-controller` resolve it (see
[producer-aware sources](/usage/producer-aware-sources/)). They can share a
cluster and namespace or stay separate; both are reconciled by Flux and both apply
under per-tenant impersonation, so the security model is consistent end to end.

## Settings you can tune

The chart wires the controller; you set Helm values. The set worth thinking about
is below — each row is the value, its default, and when you'd change it.
Everything else the chart configures for you (see
[what the chart manages](#what-the-chart-manages)).

| Helm value | Default | When to change |
|---|---|---|
| `replicas.min` / `replicas.max` | `1` / `1` | Raise both to ≥ 2 for HA; set `max > min` to also render an HPA + PDB. |
| `controller.leaderElect` | `true` | Leave on — harmless at one replica, required for HA. |
| `controller.defaultInterval` | `10m` | The reconcile cadence StageSets inherit when they omit `spec.interval`. Lower for faster drift correction cluster-wide. |
| `controller.inventoryMode` | `hybrid` | `applyset` for ApplySet-native tooling; `entries` to drop the ApplySet labels. |
| `controller.inventoryShardCap` | `5000` | Lower only if a stage applies a huge object count and you want smaller inventory objects. |
| `controller.allowedActionHosts` | `[]` | Add host globs your `http` [actions](/usage/actions/) must reach (loopback/link-local are always denied). |
| `controller.noCrossNamespaceRefs` | `false` | `true` to hard-isolate namespaces (deny cross-namespace `sourceRef`/`dependsOn`). |
| `controller.watchNamespaces` | `[]` | Restrict the controller to a namespace list (cache + RBAC pivot to per-namespace bindings); empty watches cluster-wide. See [tenancy](/usage/multi-cluster/#scoping-the-controller-to-a-namespace-set). |
| `rbac.clusterAdmin` | `false` | `true` on **single-tenant** clusters to bind the controller SA to `cluster-admin` so StageSets apply without `serviceAccountName`. See [single-tenant](/usage/multi-cluster/#single-tenant-cluster-admin). |
| `controller.runbookBaseURL` | the docs site | Point at a fork/mirror, or empty to drop the runbook links from Ready messages. |
| `webhook.certMode` | `self-signed` | `cert-manager` if you run cert-manager — see [reference setups](#reference-setups). |
| `gate.enabled` | `true` | Leave on for [progressive delivery](/tutorials/progressive-delivery/) (the Flagger/Argo gate); set `false` to drop the gate Service and endpoint. |
| `rollbackStore.backend` | `none` | `pvc` (RWX) or `s3` to enable [`spec.rollbackOnFailure`](/usage/rollback/); the two are mutually exclusive. |
| `rollbackStore.s3.sse` | `s3` | At-rest encryption for the S3 store (it holds rendered Secret data): `s3` (SSE-S3), `kms` (+`sseKmsKeyId`), or `none`. See [encryption at rest](/usage/rollback/#encryption-at-rest). |
| `networkPolicy.enabled` | `false` | `true` to fence the controller and the unauthenticated gate. |
| `metrics.serviceMonitor.enabled` | `false` | `true` if you scrape with the Prometheus operator. |
| `metrics.prometheusRule.enabled` | `false` | `true` for the bundled [alerts](/installation/operations/#alerts). |
| `serviceAccount.annotations` | `{}` | An IRSA role ARN on EKS so the S3 store uses an IAM role. |
| `namespace.create` | `false` | `true` to have the chart create the install namespace with Pod-Security labels. |
| `resources` | requests = limits | Raise for very large or very busy releases. |

Every option is set the same way — in your values file, applied with
`helm upgrade --install … -f values.yaml`. The [reference setups](#reference-setups)
above are complete, copy-pasteable examples.

## What the chart manages

You do **not** configure these — the chart wires them so the controller behaves
correctly out of the box:

- **Leader election and HA plumbing** — the lease, and the PDB/HPA when
  `replicas.max > replicas.min`.
- **The admission webhook** — the server, its Service, the
  `ValidatingWebhookConfiguration`, and the certificate (cert-manager `Certificate`
  or the in-pod self-signed renewer, per `webhook.certMode`).
- **Endpoints** — metrics, health probes, and the gate, on their Services.
- **RBAC** — the ClusterRole/bindings the controller needs, including the
  `impersonate` verb (it never applies as itself).
- **A hardened pod** — non-root, read-only root filesystem, dropped capabilities,
  seccomp `RuntimeDefault` (see [pod security context](#pod-security-context)).
- **Per-tenant impersonation** — every apply runs as the StageSet's
  `spec.serviceAccountName`.

## Controller flags

The chart sets the controller's command-line flags from your Helm values and its
own defaults — you never pass them directly. For the exhaustive per-flag list with
defaults, see the [Configuration reference](/installation/configuration/), which
also notes which Helm value drives each one.
