---
title: CLI
description: Install and use stagesetctl to preview, render, and drive StageSets from your own kubeconfig.
tags: [cli, stagesetctl, kubectl-plugin]
---

`stagesetctl` previews, renders, and drives StageSets without waiting for the next
reconcile. It speaks to the cluster with your own kubeconfig — nothing about it runs
in-cluster.

Installed on your `PATH` as `kubectl-stageset`, it also works as a kubectl plugin:
`kubectl stageset <command>` is equivalent to `stagesetctl <command>`.

## Installation

`stagesetctl` ships as a standalone binary attached to every
[GitHub release](https://github.com/metio/stageset-controller/releases) as
`stagesetctl_<version>_<os>_<arch>.tar.gz` (`.zip` on Windows), for linux, darwin,
and windows on amd64 and arm64. The binary inside the archive is named
`stagesetctl_v<version>`.

Download the archive for your platform, extract it, and install the binary onto your
`PATH`:

```shell
tar -xzf stagesetctl_<version>_<os>_<arch>.tar.gz
install -m 0755 stagesetctl_v<version> /usr/local/bin/stagesetctl
```

To use it as a kubectl plugin, expose it on your `PATH` as `kubectl-stageset`:

```shell
ln -s "$(command -v stagesetctl)" /usr/local/bin/kubectl-stageset
```

After that, `kubectl stageset <command>` works.

| Command | Purpose |
|---|---|
| [`get`](/cli/get/) | Print a StageSet's status, or list StageSets. |
| [`build`](/cli/build/) | Render a StageSet's manifests to stdout. |
| [`diff`](/cli/diff/) | Preview what a reconcile would change; usable as a CI gate. |
| [`plan`](/cli/plan/) | Preview what a reconcile would do — which actions run, skip, or re-run; usable as a CI gate. |
| [`apply`](/cli/apply/) | Server-side-apply a StageSet's rendered manifests directly. |
| [`reconcile`](/cli/reconcile/) | Force an out-of-band reconcile. |
| [`lint-migrations`](/cli/lint-migrations/) | Validate a migration ladder before publishing it to a source. |
| [`baseline`](/cli/baseline/) | Assert a once-per-lifetime action already completed, or export a ledger's completions. |
| [`reset-ledger`](/cli/reset-ledger/) | Forget a StageLedger completion so its once-per-lifetime action runs again. |

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

Two commands exit `1` for their own reason, so they can gate a CI pipeline:
[`diff`](/cli/diff/) when it finds changes (the `diff(1)` convention), and
[`lint-migrations`](/cli/lint-migrations/) when it finds a validation problem in
a migration ladder.
