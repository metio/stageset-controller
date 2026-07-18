---
title: stagesetctl diff
description: Preview what a StageSet would change in the cluster — usable as a CI gate.
tags: [cli, stages, ci]
---

By default `diff` performs a
[server-side](https://kubernetes.io/docs/reference/using-api/server-side-apply/)
dry-run apply and exits `1` when there are changes, so it works as a CI gate. It
shows, per object, what a reconcile would create, configure, or delete, plus the
[actions](/defining-a-release/actions/) a rollout would run. To see the full rendered manifests
without comparing against the cluster, use [`build`](/cli/build/).

```text
stagesetctl diff NAME [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--stage` | _(all)_ | Diff only the named stage(s); repeatable. |
| `--source-dir` | _(none)_ | Use a local artifact tree as `[STAGE=]PATH`; repeatable. Skips the cluster fetch. |
| `--server-side` | `true` | Server-side dry-run apply diff (needs update/patch RBAC). `false` renders client-side against live objects. |
| `--as-tenant` | `false` | Server-side dry-run each stage as its effective `serviceAccountName` — the identity the controller _applies_ with (the stage's own, else `spec.serviceAccountName`). Reads (source resolve, `substituteFrom`, inventory) always use your credentials, mirroring the controller, which reads as itself (see [multi-cluster and tenancy](/security/multi-cluster/)). |
| `--no-cross-namespace-refs` | `false` | Reject a stage `sourceRef` that targets another namespace, matching a controller run with `--no-cross-namespace-refs`, so the preview fails the way the controller would. |
| `--show-secrets` | `false` | Reveal Secret values instead of masking. |
| `--show-unchanged` | `false` | Include objects with no change. |
| `--prune` | `true` | Show resources that would be deleted (fell out of inventory). |
| `--color` | `auto` | Colorize output: `auto`, `always`, or `never`. |
| `--exit-code` | `true` | Exit `1` when changes are found. `false` always exits `0` on a clean run. |

## Example

```shell
stagesetctl diff payments
```

```text
--- live
+++ merged
@@ Deployment payments/web @@
 spec:
-  replicas: 3
+  replicas: 6

- ConfigMap payments/old-feature-flags (pruned: fell out of inventory)

Actions to run:
  application:
    pre   db-migrate   job ledger-migrations
    post  smoke-test   http https://payments.internal/healthz
```

Objects that left the stage's [inventory](/api/stageinventory/) show as deletions
(`pruned: …`); pass `--prune=false` to hide them. The trailing `Actions to run`
block lists the [pre/post actions](/defining-a-release/actions/) a real reconcile
would run, honoring the
[action scopes](/defining-a-release/actions/#scope-revision-version-or-lifetime):
an action already satisfied for its scope — a `Revision` action recorded at this
revision, a `Version` action held at a fixed version, a completed once-ever
`Lifetime` action — is omitted, plus the stage's `onFailure` actions. `diff` never
runs them, it only reports them. For the full behavioral preview — _why_ each
action runs, skips, or re-runs — see [`plan`](/cli/plan/).

A clean run prints nothing and exits `0`; pending changes exit `1`. To inspect
without failing the shell:

```shell
stagesetctl diff payments --color=never --exit-code=false
```

Use `--server-side=false` when you lack apply RBAC and only need a textual
render-versus-live comparison.
