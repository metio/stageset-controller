---
title: DowngradeRequiresMigration
description: A downgrade is enabled, but a version boundary it crosses declares no down actions and is irreversible.
tags: [runbooks, migrations, versioning, troubleshooting]
---

## Symptom

`READY=False`, `REASON=DowngradeRequiresMigration`. Terminal: the run does not requeue until the boundary gains a down path or the target version is raised above it.

## Cause

`spec.version.allowDowngrade` is set, so downgrades are permitted — but the transition crosses a [migration](/gating/versioned-migrations/) boundary that was applied on the way up and declares no `down` actions. That boundary is irreversible: lowering the version past it would leave the schema ahead of the code. Rather than do that silently, the controller refuses and names the migration. A downgrade only proceeds when **every** applied boundary it crosses can be unwound.

## Diagnosis

```shell
kubectl --namespace <namespace> describe stageset <name>   # the message names the irreversible migration
kubectl --namespace <namespace> get stageset <name> --output jsonpath='{.status.version}'   # deployed
kubectl --namespace <namespace> get stageset <name> --output jsonpath='{.status.executedMigrations}'   # what was applied
```

`stagesetctl plan <name>` previews the rollback and flags the irreversible boundary the same way, before you commit the version change.

## Remediation

Pick the intended direction:

- **Make the boundary reversible**: add `down` actions to the named migration that restore the prior state (e.g. a down that recreates a dropped column from backup). The downgrade then unwinds it in reverse order.
- **Do not cross it**: raise the target version to at or above the irreversible boundary, so the downgrade stops short of it.
- **You did not mean to downgrade** (a source revert pulled an older version file): roll the source forward so the desired version is `>=` the deployed version.
