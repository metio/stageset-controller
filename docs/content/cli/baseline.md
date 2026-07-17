---
title: stagesetctl baseline
description: Assert a once-per-lifetime action already completed (adoption), or export a ledger's completions as a committable baseline.
tags: [cli, actions, operations]
---

Adopts a system whose once-per-lifetime bootstrap already ran by asserting an
action complete in the
[StageLedger](/defining-a-release/actions/#run-once-ever-scope-lifetime)'s
`spec.baseline` — the controller promotes it to a recorded completion without
running the action. `--export` instead prints the ledger's current completions as
a committable `spec.baseline` snippet, for disaster recovery or renaming a ledger.

```text
stagesetctl baseline NAME [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--stage` | _(required)_ | Stage the action belongs to. |
| `--action` | _(required)_ | Name of the `scope: Lifetime` action to baseline. |
| `--export` | `false` | Print the ledger's current completions as a `spec.baseline` snippet instead of adding one. Mutually exclusive with `--stage`/`--action`. |

When the StageSet exists, the named action must be a `scope: Lifetime` action or
the command fails (a typo guard). When it does not exist yet, the command
proceeds — applying the ledger before the StageSet is the race-free adoption
order — and the controller validates the entry once the StageSet is applied.

## Examples

Adopt a running system without re-running its bootstrap:

```shell
stagesetctl baseline moodle --stage app --action install-database
```

Export completions to commit for disaster recovery:

```shell
stagesetctl baseline moodle --export > moodle-ledger.yaml
```
