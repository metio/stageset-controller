---
title: Configuration reference
description: Every stageset-controller command-line flag and its default.
tags: [installation, configuration, reference]
---

The controller is configured entirely through command-line flags, grouped below
by subsystem. When deployed via the Helm chart you never pass these directly — the
chart sets them from your values and its own defaults. For the values that drive
each flag, see [Helm chart values](/reference/helm-values/). For the values
worth tuning and the reasoning behind each, see
[Production](/running/production/); for metrics and
runbooks, [Operations](/running/operations/).

The flag tables below are generated from the binary's own FlagSet, so they track
the controller's runtime contract rather than a hand-maintained copy.

## Manager and leader election

{{< flag-table group="Manager and leader election" >}}

The leader-election lease name is fixed at `stageset-controller.stages.metio.wtf`
and is created in the namespace the controller pod runs in.

## Watch scope

{{< flag-table group="Watch scope" >}}

**Environment variable:** `STAGESET_WATCH_NAMESPACES` — comma-separated
namespace list. When `--watch-namespaces` is non-empty the flag takes
precedence. When restricted, the chart pivots RBAC to per-namespace
RoleBindings instead of a cluster-wide ClusterRoleBinding.

## Reconciliation defaults

{{< flag-table group="Reconciliation defaults" >}}

## Rollback store — filesystem

The rollback store preserves a copy of each stage's last-applied artifact so
that a rollback can re-apply the previous revision without re-fetching from the
producer. The filesystem backend is appropriate for single-replica deployments or
multi-replica deployments backed by an `RWX` volume.

`--rollback-store-path` and `--rollback-store-s3-endpoint` are mutually
exclusive. Both empty disables the store; rollback falls back to re-fetching the
producer artifact.

{{< flag-table group="Rollback store — filesystem" >}}

The file store writes rendered output — including Secret data — in the clear.
The volume must provide encryption at rest (encrypted StorageClass, LUKS, or
cloud-disk encryption).

## Rollback store — S3

Active when `--rollback-store-s3-endpoint` and `--rollback-store-s3-bucket` are
both non-empty.

{{< flag-table group="Rollback store — S3" >}}

## Metrics and health

{{< flag-table group="Metrics and health" >}}

The metrics endpoint exposes standard `controller_runtime_*` and `workqueue_*`
series alongside the custom `stageset_*` metrics documented in
[Operations](/running/operations/).

The probe endpoint serves `GET /healthz` (liveness, an unconditional `200`),
`GET /readyz` (readiness, green once the manager's caches have synced), and
`GET /manager`, which reports whether the manager is reconciling and, when it is
not, the reason, the time the reading last changed, and how many times the manager
has been started. Both endpoints are bound by the binary rather than by the
manager, so they keep answering while the manager is the thing that is down — see
[Degraded manager](/running/operations/#degraded-manager). Either address takes
`0` or an empty value to disable that endpoint.

## Tracing

The controller exports OpenTelemetry traces over OTLP gRPC when
`--tracing-endpoint` is set; it is off by default. See
[Observability](/observability/) for the collector wiring.

{{< flag-table group="Tracing" >}}

## Webhook and TLS provisioning

The validating admission webhook for `StageSet` is enabled by default. Two TLS
provisioning modes are supported: `cert-manager` (the chart renders a
`Certificate` CR and mounts the issued cert from a Secret) and `self-signed`
(the controller generates a CA and serving cert in-pod and patches the
`ValidatingWebhookConfiguration` `caBundle`).

{{< flag-table group="Webhook and TLS provisioning" >}}

## Gate endpoint

The gate endpoint exposes a read-only HTTP API for Flagger canary stage-gates.
`GET /gate/{namespace}/{stageset}/{stage}` returns `200` when the named stage is
ready to advance and `503` otherwise.

{{< flag-table group="Gate endpoint" >}}

## Logging

The controller logs through `log/slog`. Set the verbosity with `--log-level`
(`debug`, `info`, `warn`, or `error`, default `info`) and the encoding with
`--log-format` (`json` or `text`, default `json`); controller-runtime's own
logs flow through the same handler via a logr bridge. See
[Logging](/observability/logging/) for reading and filtering the output.

{{< flag-table group="Logging" >}}
