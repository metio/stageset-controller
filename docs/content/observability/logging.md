---
title: Logging
description: The controller's structured slog output, the --log-level and --log-format flags, reading logs with kubectl and jq, and the chart values that drive them.
tags: [observability, logging, slog]
---

The controller logs through Go's `log/slog`. Controller-runtime's own logs flow
through the same handler via the logr bridge, so the manager, the cache, and the
reconciler all share one consistent output stream — there is no second logger to
configure.

> **Status Conditions and Kubernetes Events stay the primary status surface** —
> `kubectl describe`, `kubectl wait --for=condition`, and Flux's
> notification-controller all read them. Logs are additive operator-side detail
> for tracing what a reconcile did, not a replacement for the condition that
> tells you whether a StageSet is Ready.

Each reconcile seeds its log lines with `namespace`, `name`, and `reconcileID`,
so every line from one pass is correlatable. The reconciler's own outcome lines
add fields describing what happened — `stages` (the stage count), `ready`, and
`requeueAfter` on synced and deferred runs, and `stage` plus `op` on a stage
failure — which is what the `jq` filters below select on.

## The controller binary

Two flags govern logging:

- `--log-level` — one of `debug`, `info`, `warn`, `error`. Defaults to `info`.
- `--log-format` — `json` or `text`. Defaults to `json`.

`json` emits one structured object per line, which is what you want for any log
pipeline (Loki, Elasticsearch, Cloud Logging). `text` is the human-readable
`key=value` form, handy when tailing logs locally.

### Reading logs

Tail the controller's logs with `kubectl`:

```shell
kubectl --namespace stageset-system logs deployment/stageset-controller --follow
```

With `--log-format=json` every line is a JSON object, so `jq` filters and
reshapes them. Show only warnings and errors:

```shell
kubectl --namespace stageset-system logs deployment/stageset-controller \
  | jq 'select(.level == "WARN" or .level == "ERROR")'
```

Project just the fields you care about onto one compact line:

```shell
kubectl --namespace stageset-system logs deployment/stageset-controller \
  | jq -c '{t: .time, level, msg, namespace, name, reconcileID, stages, ready, requeueAfter}'
```

Follow a single StageSet by `namespace` and `name`:

```shell
kubectl --namespace stageset-system logs deployment/stageset-controller --follow \
  | jq -c 'select(.namespace == "apps" and .name == "checkout")'
```

Pull every line from one reconcile pass by its `reconcileID` to read a single
run end to end:

```shell
kubectl --namespace stageset-system logs deployment/stageset-controller \
  | jq -c 'select(.reconcileID == "a1b2c3d4-...")'
```

Surface only stage failures, which carry the failing `stage` and `op`:

```shell
kubectl --namespace stageset-system logs deployment/stageset-controller \
  | jq -c 'select(.msg == "stage failed") | {namespace, name, stage, op, error}'
```

Turning `--log-level=debug` on raises the volume considerably — controller-runtime
logs every reconcile request at debug. Use it to diagnose a specific issue, then
return to `info`.

## The Helm chart

Two values map directly onto the flags:

```yaml
controller:
  logLevel: info   # debug | info | warn | error  -> --log-level
  logFormat: json  # json | text                  -> --log-format
```

Both are enum-validated in the chart's values schema, so a typo fails the install
rather than reaching the binary as an unknown level. The defaults (`info` / `json`)
are production-ready; lower the level only while investigating, and switch to
`text` only for ad-hoc local runs where a human reads the stream directly.
