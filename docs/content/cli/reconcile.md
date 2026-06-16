---
title: stagesetctl reconcile
description: Force an out-of-band reconcile, optionally waiting for it to be handled.
tags: [cli, stages, operations]
---

Stamps the `reconcile.fluxcd.io/requestedAt`
[annotation](/api/stageinventory/#well-known-labels-and-annotations) to trigger a
reconcile now, optionally waiting for the controller to report it handled.

```text
stagesetctl reconcile NAME [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--stage` | _(all)_ | Force only this stage to re-run its actions (single-stage reconcile). |
| `--with-source` | `false` | Also re-request the stage sources before reconciling. |
| `--update-now` | `false` | Apply a window-held rollout immediately, bypassing update windows. |
| `--force` | `false` | Proceed even when the StageSet is suspended. |
| `--wait` | `false` | Block until the controller reports the request handled. |
| `--timeout` | `5m` | How long to wait with `--wait`. |

## Example

```shell
stagesetctl reconcile payments -n payments
```

```text
Reconcile requested for StageSet payments (token 2026-06-15T09:30:00Z)
```

Force just one stage to re-run its actions:

```shell
stagesetctl reconcile payments --stage application
```

```text
Reconcile requested for stage "application" of StageSet payments (token 2026-06-15T09:31:12Z)
```

Re-pull sources, push a window-held rollout through, and wait for it:

```shell
stagesetctl reconcile payments --with-source --update-now --wait --timeout 10m
```

`--update-now` is the CLI equivalent of the `stages.metio.wtf/update-now`
annotation — the supported escape hatch when an [update
window](/usage/update-windows/) is holding a rollout you need to ship now.
