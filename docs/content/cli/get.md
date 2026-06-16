---
title: stagesetctl get
description: Print human-readable StageSet status, or list StageSets.
tags: [cli, stages, operations]
---

With no `NAME`, lists StageSets in the current namespace. With a `NAME`, prints that
StageSet's detail (Ready reason, per-stage phase, revisions, version) — a readable
view of [`StageSet.status`](/api/stageset/#status).

```text
stagesetctl get [NAME] [flags]
```

| Flag | Default | Description |
|---|---|---|
| `-A`, `--all-namespaces` | `false` | List StageSets across all namespaces. |
| `-o`, `--output` | _(table)_ | Output format: empty for the human table, or `yaml` / `json`. |

## Listing

```shell
stagesetctl get -A
```

```text
NAMESPACE   NAME       READY   REASON       STAGES   VERSION   PENDING
payments    payments   True    Succeeded    2/2      2.1.0     -
platform    platform   True    Succeeded    3/3      -         -
staging     web        False   StageFailed  1/2      -         -
```

`STAGES` is `ready/total`; `PENDING` shows `held until <time>` when an
[update window](/usage/update-windows/) is holding a rollout. A `False` `READY`
maps to a [runbook](/runbooks/) by its `REASON`.

## Detail

```shell
stagesetctl get payments -n payments
```

```text
Name:       payments
Namespace:  payments
Ready:      True (Succeeded)
Message:    All 2 stages applied
Version:    2.1.0
Last handled reconcile: 2026-06-15T09:21:04Z
Stages:
  NAME            PHASE   REVISION        ENTRIES
  infrastructure  Ready   sha256:9f3c1a   12
  application     Ready   sha256:1a2b3c   8
```

Conditional lines fill in when the StageSet is in that state: `Suspended: true`
when [`spec.suspend`](/api/stageset/#scheduling) is set, `Pending migrations:`
when a [version boundary](/usage/versioned-migrations/) is queued, and a
`Pending update:` block (next-window time plus the held revisions) when an
[update window](/usage/update-windows/) is holding a rollout — for example:

```text
Ready:      False (UpdateDeferred)
Pending update:
  Next window opens: 2026-06-16T08:00:00Z
  Held: payments/payments-app -> sha256:cafe
```

Add `-o yaml` (or `-o json`) to print the full object instead of the summary — the
machine-readable form for scripting or piping into `jq`/`yq`.
