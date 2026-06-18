---
title: Production
description: A decision-oriented checklist for hardening a stageset-controller install before it manages production releases.
tags: [production, security, operations, ha]
---

The chart's defaults bring up a hardened, lease-guarded controller, but a few
decisions still belong to you before it manages production releases: whether
rollback is enabled and where its store lives, how many replicas run, what the
controller can reach, and how you observe and recover it. Work through these in
order. Each step states what to set and why, and ends with the Helm values for
that step. Every flag and its Helm value live in the [configuration
reference](/installation/configuration/).

## 1. Decide on rollback and pick its store

Rollback is opt-in. With no store the controller leaves a failed run on the
broken release; with one it restores the last successfully-applied artifact
revisions. The store keeps bit-exact copies that outlive the producer's garbage
collection, so it is the difference between a recoverable failure and a manual
incident.

Two mutually-exclusive backends are available — a `ReadWriteMany` PVC or an
S3-compatible bucket. For HA the store must be reachable from whichever pod holds
the lease, which rules out a `ReadWriteOnce` PVC. On-prem, use an RWX class (NFS,
CephFS); on a cloud provider, use the object store. The decision, the keep-count,
and how restore selects a revision are covered in [Rollback](/usage/rollback/).

The store holds rendered `Secret` data, so encrypt it at rest. The S3 backend
takes a server-side-encryption mode (SSE-S3 or SSE-KMS with your key); see
[encryption at rest](/usage/rollback/#encryption-at-rest). This is distinct from
SOPS decryption of a stage's own source, which is configured per stage in
[Secrets encryption](/usage/encryption/).

```yaml
# Option A: RWX PVC (on-prem)
rollbackStore:
  backend: pvc
  pvc:
    accessModes: [ReadWriteMany]
    storageClass: nfs-client     # your RWX class (NFS, CephFS, …)
    size: 10Gi

# Option B: S3-compatible bucket (cloud)
rollbackStore:
  backend: s3
  s3:
    endpoint: s3.eu-west-1.amazonaws.com
    bucket: my-org-stageset-rollback
    region: eu-west-1
    sse: s3                      # SSE-S3; or kms (+ sseKmsKeyId), or none
```

## 2. Run leader-elected HA {#high-availability}

The chart enables leader election by default, so even a single replica is
lease-guarded. For HA, raise the replica floor above one: only the lease holder
reconciles, while every replica answers admission webhook calls, so admission
stays available during a leader handover. When the replica ceiling exceeds the
floor the chart also renders a `HorizontalPodAutoscaler` (CPU target 80%) and a
`PodDisruptionBudget` (`minAvailable: 1`).

The lease is not released eagerly on shutdown, so after a rolling update the new
leader takes over when the old lease expires — budget a few seconds of reconcile
pause on restart. Admission and the gate endpoint stay available throughout,
since every replica serves them. The HA model is detailed in
[multi-cluster and tenancy](/usage/multi-cluster/).

```yaml
replicas:
  min: 2                 # leader-elected HA; the non-leader still serves admission
  max: 3                 # > min renders an HPA (CPU 80%) and a PodDisruptionBudget

controller:
  leaderElect: true
```

## 3. Choose the tenancy model and scope RBAC

The controller never applies your manifests as itself. Every cluster operation
for a `StageSet` — building, applying, pruning, running actions — runs as the
`StageSet`'s `spec.serviceAccountName`, so a release can only do what its tenant
`ServiceAccount` permits and an over-broad or missing SA fails closed. On the
local cluster the controller assumes that identity by minting a short-lived
TokenRequest token for the SA, so the chart grants it only `create` on
`serviceaccounts/token` — not write access and not the `impersonate` verb. Give
every production `StageSet` a scoped `ServiceAccount`.

Pick the model that matches your cluster. Multi-tenant clusters keep
impersonation on and may scope the controller to a namespace set — one instance
per tenant group, which also pivots RBAC to per-namespace bindings. Single-tenant
clusters can bind the controller to `cluster-admin` so StageSets apply without a
`serviceAccountName`. Both models, and remote-cluster reconciliation, are in
[multi-cluster and tenancy](/usage/multi-cluster/). To hard-isolate namespaces,
deny cross-namespace `sourceRef`/`dependsOn` references.

```yaml
# Multi-tenant: scope the controller to a namespace set (RBAC pivots to
# per-namespace bindings) and hard-isolate cross-namespace references.
controller:
  watchNamespaces: [team-a, team-b]
  noCrossNamespaceRefs: true

# Single-tenant: bind the controller SA to cluster-admin so StageSets apply
# without a per-release serviceAccountName.
rbac:
  clusterAdmin: true
```

## 4. Set resources for your release sizes

The chart constrains the pod with requests equal to limits, so it is fully
bounded out of the box. The defaults suit ordinary workloads; raise them for very
large inventories or very busy clusters. Ephemeral storage covers `/tmp` and the
self-signed cert directory, both emptyDirs. The reconcile cadence StageSets
inherit when they omit `spec.interval`, and the inventory mode and shard cap that
govern how applied objects are tracked, are tuning knobs worth reviewing.

```yaml
resources:
  cpu: 50m
  memory: 256Mi
  ephemeralStorage: 32Mi   # /tmp and the self-signed cert dir are emptyDirs

controller:
  defaultInterval: 10m     # cadence StageSets inherit when they omit spec.interval
  inventoryMode: hybrid    # applyset for ApplySet-native tooling; entries to drop the labels
  inventoryShardCap: 5000  # lower for smaller inventory objects on huge applies
```

## 5. Fence the network and the gate {#network-policy}

The gate endpoint is unauthenticated — a read-only `GET` that progressive-delivery
tooling (Flagger, Argo Rollouts) polls. Turn on the ingress-only `NetworkPolicy`
to fence the gate, webhook, and metrics ports to only the peers that need them.

The policy is ingress-only and does not restrict egress. The controller still
fetches stage artifacts over HTTP from source-controller, so if your cluster
default-denies egress, add an egress allowance to source-controller and DNS. When
a stage runs `http` [actions](/usage/actions/), the hosts those actions may reach
are an explicit allow-list — loopback and link-local are always denied.

```yaml
networkPolicy:
  enabled: true              # admits the webhook (9443), metrics (8080), gate (8082)

controller:
  allowedActionHosts:        # host globs http actions may reach; loopback/link-local always denied
    - "*.internal.example.com"
```

## 6. Provision the admission webhook certificate {#admission-webhook-tls}

The webhook validates `StageSet` spec invariants at `kubectl apply` time instead
of at reconcile time. It is wired by the chart; the one decision is how its
serving certificate is obtained. The default `self-signed` mode generates an
in-pod CA and serving cert and rotates them at a third of their validity with no
external dependency. The `cert-manager` mode hands issuance and rotation to
[cert-manager](https://cert-manager.io/), which must be installed in the cluster.

```yaml
# Option A: cert-manager (issues + rotates the cert; requires cert-manager)
webhook:
  certMode: cert-manager
  certManager:
    issuerRef:
      kind: ClusterIssuer
      name: letsencrypt-prod

# Option B: self-signed (chart default: in-pod CA + serving cert, rotated at
# validity/3, with no cert-manager dependency)
webhook:
  certMode: self-signed
```

## 7. Enable observability and alerts

Turn on the `ServiceMonitor` if you scrape with the Prometheus operator, and the
bundled `PrometheusRule` for the starter alert set. Match the Prometheus selector
labels so your instance picks up each object. The exposed metrics, the shipped
alerts, and the events the controller emits are documented in
[Operations](/installation/operations/#metrics).

```yaml
metrics:
  serviceMonitor:
    enabled: true
    additionalLabels:
      release: kube-prom       # match your Prometheus's serviceMonitorSelector
  prometheusRule:
    enabled: true
    labels:
      release: kube-prom       # match your Prometheus's ruleSelector
    extraAlertLabels:
      team: platform           # Alertmanager routing label
```

Every actionable Ready-condition reason has a [runbook](/runbooks/), and the
controller appends a direct link to each Ready message
(`(runbook: https://stageset.projects.metio.wtf/runbooks/<reason>/)`), so a
`kubectl describe` on a failing StageSet routes straight to the fix. Healthy
reasons get no link.

## 8. Plan for upgrades and recovery

Calendar-based releases run on a weekly cron. Chart upgrades are `helm upgrade
--install`; the chart ships CRDs under `templates/`, so schema changes apply
automatically. Read
[MIGRATIONS.md](https://github.com/metio/stageset-controller/blob/main/MIGRATIONS.md)
before each upgrade — a release that changes an immutable selector field requires
a manual delete first. For day-two work — forcing a reconcile, drift correction,
reading events, and following a runbook — see
[Operations](/installation/operations/).

## A complete production values.yaml

Two HA setups share one backbone: a leader-elected pair (or trio), a rollback
store reachable from whichever pod holds the lease, cert-manager for the webhook,
a `NetworkPolicy` fencing the unauthenticated gate, and a `ServiceMonitor` for
Prometheus. They differ only in where the rollback store lives.

### On-prem (RWX storage)

Back the store with a `ReadWriteMany` PVC on your storage class (NFS, CephFS, …)
so every replica mounts the same volume — the store must be reachable from the
lease holder, and an RWO PVC cannot satisfy that under HA.

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

### AWS / EKS (S3 + IRSA)

Back the store with S3 and let the controller assume an IAM role through
[IRSA](https://docs.aws.amazon.com/eks/latest/userguide/iam-roles-for-service-accounts.html)
instead of static keys: annotate the controller's ServiceAccount with the role
ARN and leave the S3 credentials empty, and the store's minio-go client picks the
role up from the pod's web-identity token. Grant the role
`s3:GetObject`/`PutObject`/`ListBucket`/`DeleteObject` on the bucket.

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
    sse: s3
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

## Next steps

- [Configuration reference](/installation/configuration/) — every flag, its
  default, and the Helm value that drives it.
- [Operations](/installation/operations/) — metrics, alerts, events, forcing a
  reconcile, drift correction.
- [Rollback](/usage/rollback/) — store backends, keep-count, and revision
  selection.
- [Multi-cluster and tenancy](/usage/multi-cluster/) — tenant ServiceAccounts,
  namespace scoping, remote-cluster reconciliation.
- [Runbooks](/runbooks/) — incident response.
