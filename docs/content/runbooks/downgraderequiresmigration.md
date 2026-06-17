---
title: DowngradeRequiresMigration
description: The desired version is below the deployed version and a migration boundary blocks the downgrade.
tags: [runbooks, migrations, versioning, troubleshooting]
---

## Symptom

`READY=False`, `REASON=DowngradeRequiresMigration`. Terminal: the run does not requeue until the desired version is at or above `status.version`.

## Cause

The desired version (`spec.version`) is **lower** than the version the controller last recorded as deployed (`status.version`). Downgrades are refused by default: [migrations](/usage/versioned-migrations/) are forward-only action ladders, and replaying upgrade migrations in reverse is how data gets destroyed. The controller does not silently run a downgrade.

## Diagnosis

```shell
kubectl --namespace <namespace> describe stageset <name>
kubectl --namespace <namespace> get stageset <name> --output jsonpath='{.status.version}'   # deployed
# desired: read spec.version.value, or the version file the artifact carries
```

## Remediation

Pick the intended direction:

- **You did not mean to downgrade** (e.g. a source revert pulled an older version file): roll the source forward again so the desired version is `>=` the deployed version. The StageSet converges normally.
- **You genuinely need to go back**: a downgrade is an operational decision with potential data loss. Perform it deliberately — restore from backup or apply an explicit down-migration out of band — then set `status.version` to match. There is no automatic reverse-migration path by design.
