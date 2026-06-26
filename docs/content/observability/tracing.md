---
title: Tracing
description: OpenTelemetry tracing for the controller — the --tracing-endpoint, --tracing-insecure, and --tracing-sample-ratio flags, pointing at an OTLP collector, and the chart values that drive them.
tags: [observability, tracing, opentelemetry, otlp]
---

The controller integrates with OpenTelemetry and exports spans over OTLP gRPC.
Tracing is **off until you set an endpoint** — with no endpoint the OTel SDK
installs a no-op tracer provider, so the instrumentation costs a function
indirection and nothing ships off the pod. The controller takes no opinion on
whether you run a collector.

## The controller binary

Three flags govern tracing:

- `--tracing-endpoint` — the OTLP gRPC collector as `host:port`, e.g.
  `otel-collector.observability.svc:4317`. Empty (the default) disables tracing
  entirely.
- `--tracing-insecure` — skip TLS when dialing the collector. Use only for
  in-cluster collectors that do not terminate TLS themselves. Defaults to `false`,
  so the dial uses TLS against the system trust store.
- `--tracing-sample-ratio` — TraceID-ratio sampling between `0.0` and `1.0`.
  Defaults to `1.0`, which samples every trace. Lower it on a busy controller to
  cap exporter and collector load.

Every span carries a `service.name` of `stageset-controller` and a
`service.version` resource attribute filled from the controller's build version,
so the collector can group spans by release.

> **Status Conditions and Kubernetes Events stay the primary status surface** —
> `kubectl describe`, `kubectl wait --for=condition`, and Flux's
> notification-controller all read them. Traces are additive operator-side detail
> for understanding *where* a reconcile spent its time, not a replacement for the
> condition that tells you *whether* a StageSet is Ready.

### The span tree

A single reconcile produces one trace rooted at **`StageSet.Reconcile`**, with a
child span per phase nested underneath. The root carries `stageset.namespace`,
`stageset.name`, and `stageset.generation` attributes; the per-stage spans carry
a `stage` attribute naming the stage. A failure on any phase records the error on
its span and marks the span's status as an error before it ends, so a failed
reconcile shows you the exact phase that broke.

- **`StageSet.Reconcile`** — the root span, open for the whole reconcile.
- **`stageset.gateWindows`** — evaluating update windows to decide whether the
  rollout may proceed now.
- **`stageset.planMigrations`** — planning version migrations across stages.
- **`stageset.buildDecryptor`** — constructing the SOPS decryptor for the run.
- **`stage.fetch`** — fetching the artifact for a stage; also carries
  `stage.revision` and `stage.digest` attributes pinning the exact source the
  stage applied.
- **`stage.decrypt`** — decrypting the fetched files for a stage.
- **`stage.build`** — building the stage's objects from the decrypted files.
- **`stage.apply`** — applying the built objects; also carries a
  `stage.objectCount` attribute.
- **`stageset.rollback`** — attempting a rollback to the last-applied revisions
  after a failed run.

The four `stage.*` spans repeat once per stage, so a multi-stage StageSet shows
several `stage.fetch` → `stage.decrypt` → `stage.build` → `stage.apply` groups in
order under the root.

### Pointing at a collector and viewing spans

Set the endpoint to an OTLP gRPC receiver — an OpenTelemetry Collector, or any
backend that speaks OTLP (Jaeger, Tempo, Grafana Cloud). A typical in-cluster
collector exposes gRPC on port `4317`:

```shell
--tracing-endpoint=otel-collector.observability.svc:4317
--tracing-insecure   # in-cluster collector with no TLS
```

Once spans flow into the collector, open its UI (Jaeger, Grafana Tempo's trace
explorer, etc.) and filter by `service.name = stageset-controller` to see the
controller's traces. Spans propagate W3C trace context, so a collector that also
receives spans from your apiserver or source-controller can stitch a reconcile to
its upstream cause.

When the controller runs under a NetworkPolicy, the OTLP collector egress must be
allowed; see the [network policy page](/security/network-policy/) for the egress row.

## The Helm chart

The tracing flags are driven by `controller.tracing`:

```yaml
controller:
  tracing:
    # OTLP gRPC collector host:port. Empty (default) disables tracing entirely.
    endpoint: otel-collector.observability.svc:4317
    # Skip TLS for an in-cluster collector that doesn't terminate TLS itself.
    insecure: true
    # TraceID-ratio sampling (0.0..1.0). 1.0 samples every trace.
    sampleRatio: 1.0
```

The chart threads these into the deployment only when `endpoint` is non-empty, so
leaving the default empty endpoint renders no tracing flags at all and the
controller stays in no-op mode. `sampleRatio` is schema-bounded to the `0.0..1.0`
range. For the full flag list with defaults, see the
[configuration reference](/reference/configuration/).
