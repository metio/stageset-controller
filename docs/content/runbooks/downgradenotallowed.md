---
title: DowngradeNotAllowed
description: The desired version is below the deployed version and downgrades are not enabled.
tags: [runbooks, migrations, versioning, troubleshooting]
---

## Symptom

`READY=False`, `REASON=DowngradeNotAllowed`. Terminal: the run does not requeue until the desired version is at or above `status.version`, or downgrades are enabled.

## Cause

The desired version (`spec.version`) is **lower** than the version the controller last recorded as deployed (`status.version`), and `spec.version.allowDowngrade` is not set. Downgrades are off by default so that a mistaken revert of a version bump — a source that rolled an older version file back into place — cannot silently unwind a schema. The controller does not lower the version until you say so.

## Diagnosis

```shell
kubectl --namespace <namespace> describe stageset <name>
kubectl --namespace <namespace> get stageset <name> --output jsonpath='{.status.version}'   # deployed
# desired: read spec.version.value, or the version file the artifact carries
```

## Remediation

Pick the intended direction:

- **You did not mean to downgrade**: roll the source forward again so the desired version is `>=` the deployed version. The StageSet converges normally.
- **You genuinely need to roll back**: set `spec.version.allowDowngrade: true`. The downgrade then runs each crossed migration's [`down` actions](/gating/versioned-migrations/) in reverse to unwind the schema before lowering the version. Preview it first with `stagesetctl plan <name>`, which lists what reverses and flags any boundary that cannot. A boundary with no `down` path surfaces as [DowngradeRequiresMigration](/runbooks/downgraderequiresmigration/) instead of running.
