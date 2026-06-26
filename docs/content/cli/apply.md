---
title: stagesetctl apply
description: Server-side-apply a StageSet's rendered manifests to the cluster, in stage order, under the controller's field manager.
tags: [cli, stages]
---

`apply` renders a StageSet's stages with the controller's resolve→fetch→build path
and [server-side-applies](https://kubernetes.io/docs/reference/using-api/server-side-apply/)
the resulting objects in stage order, under the controller's field manager and owner
labels — so a later reconcile sees no drift. Preview first with [`diff`](/cli/diff/).

`apply` materializes the manifests only. It does **not** run stage
[actions](/defining-a-release/actions/), [migrations](/gating/versioned-migrations/),
[update-window](/gating/update-windows/) gating, ready checks (beyond `--wait`), or
[pruning](/defining-a-release/inventory/) — those belong to the controller's reconcile loop. When
a controller manages this StageSet it stays the source of truth and reconciles on its
own schedule; reach for `apply` in cluster-free workflows (no controller installed)
and break-glass.

```text
stagesetctl apply NAME [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--stage` | _(all)_ | Apply only the named stage(s); repeatable. |
| `--source-dir` | _(none)_ | Use a local artifact tree as `[STAGE=]PATH`; repeatable. Skips the cluster fetch. |
| `--as-tenant` | `false` | Render and apply impersonating `spec.serviceAccountName` (see [multi-cluster and tenancy](/security/multi-cluster/)). |
| `--wait` | `false` | Wait for each stage's objects to become ready before applying the next stage. |
| `--timeout` | `5m` | Per-stage readiness timeout with `--wait`. |

`apply` needs apply/patch RBAC for every object it writes. With `--as-tenant` that
RBAC is the tenant ServiceAccount's, exactly as a reconcile applies.

## Example

```shell
stagesetctl diff payments      # preview
stagesetctl apply payments     # then apply
```

```text
stage "infrastructure":
  created ConfigMap/payments/db-config
  configured Deployment/payments/postgres
stage "application":
  unchanged ConfigMap/payments/web-config
  configured Deployment/payments/web
applied 4 object(s) across 2 stage(s)
```

Because `apply` does not prune, removing an object from a stage's source does not
delete it from the cluster — only the controller's [inventory](/api/stageinventory/)
prune does that. Every applied object carries the `stages.metio.wtf/stage` label, so
without a controller you can find and remove a stage's objects yourself:

```shell
kubectl --namespace payments get all,configmap,secret --selector stages.metio.wtf/stage=application
```
