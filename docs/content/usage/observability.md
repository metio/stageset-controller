---
title: Observability
description: The four observability pillars for the controller — structured logging, OTLP tracing, Prometheus metrics, and the opt-in alert catalog — and where each is configured.
tags: [observability, logging, tracing, metrics, alerts]
---

> **Status Conditions and Kubernetes Events are the primary status surface** —
> `kubectl describe`, `kubectl wait --for=condition`, and Flux's
> notification-controller all read them. The four pillars below are additive
> operator-side detail; they explain how a reconcile behaved, but the condition
> on the StageSet is what tells you whether it is Ready.

The controller surfaces its behaviour through four pillars, each configured by a
small set of binary flags that the Helm chart drives from values:

- **[Logging](/usage/logging/)** — structured `log/slog` output (JSON or text)
  for the controller and, through the logr bridge, controller-runtime itself.
  Read it with `kubectl logs` and filter with `jq`.
- **[Tracing](/usage/tracing/)** — OpenTelemetry spans exported over OTLP gRPC to
  a collector you point the controller at. Off until you set an endpoint.
- **[Metrics](/usage/metrics/)** — a `stageset_*` family of Prometheus series on
  reconcile outcomes, stage applies, drift correction, deferred rollouts, and
  per-stage readiness, alongside the controller-runtime and workqueue series.
  Scrape with a `ServiceMonitor` or a plain scrape config and query with PromQL.
- **[Alerting](/usage/alerting/)** — an opt-in `PrometheusRule` with a starter
  alert set whose thresholds are tunable values, plus the Kubernetes Events the
  controller emits on StageSet transitions. Every alert links to a
  [runbook](/runbooks/).

Each page is split into two halves: what the controller binary does and how you
read or query it, and the exact Helm chart values that drive it. The full flag
list with defaults is on the
[configuration reference](/installation/configuration/).
