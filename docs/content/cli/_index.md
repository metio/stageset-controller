---
title: CLI
---

`stagesetctl` previews, renders, and drives StageSets without waiting for the next
reconcile. It speaks to the cluster with your own kubeconfig — nothing about it runs
in-cluster.

Installed on your `PATH` as `kubectl-stageset`, it also works as a kubectl plugin:
`kubectl stageset <command>` is equivalent to `stagesetctl <command>`.

| Command | Purpose |
|---|---|
| [`get`](/cli/get/) | Print a StageSet's status, or list StageSets. |
| [`build`](/cli/build/) | Render a StageSet's manifests to stdout. |
| [`diff`](/cli/diff/) | Preview what a reconcile would change; usable as a CI gate. |
| [`reconcile`](/cli/reconcile/) | Force an out-of-band reconcile. |

## Global flags

Every command accepts the standard kubectl connection flags
(`genericclioptions.ConfigFlags`): `--kubeconfig`, `--context`, `-n/--namespace`,
`--as`, `--as-group`, `--server`, `--token`, `--request-timeout`, and the rest.
`--version` prints the binary version and commit (`<version> (commit <commit>)`).
With no `-n/--namespace`, the command uses the namespace from your current
kubeconfig context, falling back to `default`.

## Exit codes

Every command shares the same baseline:

| Code | Meaning |
|---|---|
| `0` | Success. |
| `2` | Usage or flag error. |
| `3` | Runtime error. |

[`diff`](/cli/diff/) adds one more: it exits `1` when it finds changes (the
`diff(1)` convention), so it can gate a CI pipeline.
