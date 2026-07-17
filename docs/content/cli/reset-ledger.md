---
title: stagesetctl reset-ledger
description: Forget a StageLedger completion so its once-per-lifetime action runs again.
tags: [cli, actions, operations]
---

Removes a recorded completion from a
[StageLedger](/defining-a-release/actions/#run-once-ever-scope-lifetime) so its
`scope: Lifetime` action is no longer suppressed and runs on the next reconcile.
The matching `spec.baseline` assertion is removed first, so a `Baselined`
completion is not immediately re-promoted.

```text
stagesetctl reset-ledger NAME [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--stage` | _(required unless `--all`)_ | Stage of the completion to forget. |
| `--action` | _(required unless `--all`)_ | Action of the completion to forget. |
| `--all` | `false` | Forget every completion in the ledger. Mutually exclusive with `--stage`/`--action`. |

This re-runs a once-ever bootstrap, so use it when the underlying state was
actually reset, or to repurpose a leftover ledger's name after a delete and
recreate signalled `LedgerAdopted`.

## Example

```shell
stagesetctl reset-ledger moodle --stage app --action install-database
```
