---
title: Configuration reference
description: Every stageset-controller command-line flag and its default.
tags: [installation, configuration, reference]
---

The controller is configured entirely through command-line flags, grouped below
by subsystem. When deployed via the Helm chart you never pass these directly — the
chart sets them from your values and its own defaults; each section notes the Helm
value that drives a flag, and the
[metio/helm-charts](https://github.com/metio/helm-charts/tree/main/charts/stageset-controller)
repo carries the full values reference. For the Helm values worth tuning and the
reasoning behind each, see [Production](/installation/production/#settings-you-can-tune);
for metrics and runbooks, [Operations](/installation/operations/).

## Manager and leader election

| Flag | Default | Description | Helm value |
|---|---|---|---|
| `--health-probe-bind-address` | `:8081` | Address the liveness and readiness probe endpoints bind to. | _chart-managed_ |
| `--leader-elect` | `false` | Enable controller-runtime leader election so only one replica reconciles at a time. Recommended for HA deployments. | `controller.leaderElect` |

The leader-election lease name is fixed at `stageset-controller.stages.metio.wtf`
and is created in the namespace the controller pod runs in.

## Watch scope

| Flag | Default | Description | Helm value |
|---|---|---|---|
| `--watch-namespaces` | _(empty)_ | Comma-separated list of namespaces the controller watches. Empty (the default) means cluster-wide. When set, the manager's cache only observes StageSets and sources in these namespaces — the multi-tenant controller-instances pattern. Falls back to the `STAGESET_WATCH_NAMESPACES` environment variable when the flag is empty. | `controller.watchNamespaces` |

**Environment variable:** `STAGESET_WATCH_NAMESPACES` — comma-separated
namespace list. When `--watch-namespaces` is non-empty the flag takes
precedence. When restricted, the chart pivots RBAC to per-namespace
RoleBindings instead of a cluster-wide ClusterRoleBinding.

## Reconciliation defaults

| Flag | Default | Description | Helm value |
|---|---|---|---|
| `--default-interval` | `10m` | Reconcile cadence for StageSets that omit `spec.interval`. | `controller.defaultInterval` |
| `--inventory-mode` | `hybrid` | Inventory strategy for tracking applied resources: `entries`, `hybrid`, or `applyset`. | `controller.inventoryMode` |
| `--inventory-shard-cap` | `5000` | Maximum number of resource entries per `StageInventory` shard. | `controller.inventoryShardCap` |
| `--no-cross-namespace-refs` | `false` | Deny `sourceRef` and `dependsOn` references that target a different namespace. | `controller.noCrossNamespaceRefs` |
| `--allowed-action-hosts` | _(empty)_ | Host glob allowed for `http` actions; repeatable. Loopback and link-local ranges are always denied unless explicitly listed. | `controller.allowedActionHosts` |
| `--runbook-base-url` | _(empty)_ | URL prefix appended to actionable Ready condition messages as `(runbook: <base>/<reason>/)`. Empty disables. | `controller.runbookBaseURL` |

## Rollback store — filesystem

The rollback store preserves a copy of each stage's last-applied artifact so
that a rollback can re-apply the previous revision without re-fetching from the
producer. The filesystem backend is appropriate for single-replica deployments or
multi-replica deployments backed by an `RWX` volume.

`--rollback-store-path` and `--rollback-store-s3-endpoint` are mutually
exclusive. Both empty disables the store; rollback falls back to re-fetching the
producer artifact.

| Flag | Default | Description | Helm value |
|---|---|---|---|
| `--rollback-store-path` | _(empty)_ | Filesystem directory (e.g. an RWX PVC mount) for the rollback store. Empty disables the filesystem backend. | `rollbackStore.backend: pvc` |

The file store writes rendered output — including Secret data — in the clear.
The volume must provide encryption at rest (encrypted StorageClass, LUKS, or
cloud-disk encryption).

## Rollback store — S3

Active when `--rollback-store-s3-endpoint` and `--rollback-store-s3-bucket` are
both non-empty.

| Flag | Default | Description | Helm value |
|---|---|---|---|
| `--rollback-store-s3-endpoint` | _(empty)_ | S3-compatible endpoint (`host:port`, e.g. `s3.amazonaws.com` or `minio.minio.svc:9000`). Empty disables the S3 backend. | `rollbackStore.s3.endpoint` |
| `--rollback-store-s3-bucket` | _(empty)_ | S3 bucket for the rollback store. Must already exist. | `rollbackStore.s3.bucket` |
| `--rollback-store-s3-prefix` | _(empty)_ | Optional object-key prefix so the rollback store can coexist with other tenants in one bucket. | `rollbackStore.s3.prefix` |
| `--rollback-store-s3-region` | _(empty)_ | S3 region. Required for AWS multi-region buckets; ignored by most S3-compatible servers. | `rollbackStore.s3.region` |
| `--rollback-store-s3-use-ssl` | `true` | Use HTTPS to talk to the S3 endpoint. Set to `false` only for local MinIO over plain HTTP. | `rollbackStore.s3.useSSL` |
| `--rollback-store-s3-access-key` | _(empty)_ | Static access key. Empty engages minio-go's IAM/IRSA credential discovery chain (env → web-identity → EC2/EKS metadata). | `rollbackStore.s3.existingSecret` |
| `--rollback-store-s3-secret-key` | _(empty)_ | Secret key, paired with `--rollback-store-s3-access-key`. | `rollbackStore.s3.existingSecret` |
| `--rollback-store-s3-session-token` | _(empty)_ | Optional session token for temporary credentials (e.g. IRSA). | `rollbackStore.s3.existingSecret` |
| `--rollback-store-s3-anonymous` | `false` | Skip request signing. For public buckets only. | `rollbackStore.s3.anonymous` |
| `--rollback-store-s3-sse` | `s3` | Server-side encryption for stored objects: `none`, `s3` (SSE-S3), or `kms` (SSE-KMS). The store holds rendered Secret data, so encryption is on by default. Set `none` only for a bucket whose backend cannot honor an SSE header. | `rollbackStore.s3.sse` |
| `--rollback-store-s3-sse-kms-key` | _(empty)_ | KMS key ARN or ID for `--rollback-store-s3-sse=kms`. Empty uses the bucket's default KMS key. | `rollbackStore.s3.sseKmsKeyId` |

## Metrics and health

| Flag | Default | Description | Helm value |
|---|---|---|---|
| `--metrics-bind-address` | `:8080` | Address the controller-runtime Prometheus metrics endpoint binds to. `"0"` disables. | _chart-managed_ |

The metrics endpoint exposes standard `controller_runtime_*` and `workqueue_*`
series alongside the custom `stageset_*` metrics documented in
[Operations](/installation/operations/).

## Webhook and TLS provisioning

The validating admission webhook for `StageSet` is enabled by default. Two TLS
provisioning modes are supported.

| Flag | Default | Description | Helm value |
|---|---|---|---|
| `--enable-webhook` | `true` | Enable the validating admission webhook for `StageSet`. | _chart-managed_ |
| `--webhook-cert-mode` | `cert-manager` | TLS provisioning mode: `cert-manager` (chart renders a `Certificate` CR; cert is mounted from a Secret) or `self-signed` (the controller generates a CA and serving cert in-pod and patches the `ValidatingWebhookConfiguration` `caBundle`). | `webhook.certMode` |
| `--webhook-cert-dir` | `/tmp/k8s-webhook-server/serving-certs` | Directory holding `tls.crt` and `tls.key` for the webhook server. | _chart-managed_ |
| `--webhook-port` | `9443` | Port the validating webhook server binds to. | _chart-managed_ |
| `--webhook-cert-validity` | `8760h` (1 year) | Validity of the self-signed serving cert. The controller rotates it every `validity/3`. | `webhook.*` |
| `--webhook-service-name` | `stageset-controller-webhook` | Kubernetes Service the webhook is reachable through. Used to build cert SANs in `self-signed` mode. | _chart-managed_ |
| `--webhook-service-namespace` | _(empty)_ | Namespace of the webhook Service. Empty falls back to the in-cluster ServiceAccount namespace. | _chart-managed_ |
| `--webhook-validating-config-name` | _(empty)_ | Name of the `ValidatingWebhookConfiguration` whose `caBundle` the controller patches. Required when `--webhook-cert-mode=self-signed`. | _chart-managed_ |

## Gate endpoint

The gate endpoint exposes a read-only HTTP API for Flagger canary stage-gates.
`GET /gate/{namespace}/{stageset}/{stage}` returns `200` when the named stage is
ready to advance and `503` otherwise.

| Flag | Default | Description | Helm value |
|---|---|---|---|
| `--gate-bind-address` | `:8082` | Address for the Flagger stage-gate endpoint. Empty disables the endpoint. | `gate.enabled` |

## Logging

Logging is powered by the controller-runtime `zap` logger. The standard zap
flags (`--zap-log-level`, `--zap-encoder`, `--zap-stacktrace-level`,
`--zap-time-encoding`, and `--zap-devel`) are available and bound to
`flag.CommandLine`; run `stageset-controller --help` to see their current
defaults.
