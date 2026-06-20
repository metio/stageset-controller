---
title: MigrationApprovalPending
description: A version transition with pending migrations is held for operator approval; approve the target version to proceed.
tags: [runbooks, migrations, approvals]
---

## Symptom

`READY=False`, `REASON=MigrationApprovalPending`. The Message names the target
version and how many migrations await approval. `status.pendingMigrations` lists
exactly what will run. Nothing is applied and `status.version` is not advanced —
this is an intentional hold, not an error.

## Cause

`spec.version.requireApproval` is set, and the current transition has pending
migrations that have not been approved. So destructive migrations don't run
unattended, the whole rollout (app and migrations) waits until an operator
authorizes the target version.

## Fix

Review `status.pendingMigrations` (or `stagesetctl diff`) to see what will run,
then approve the target version by setting the annotation to it:

```shell
kubectl annotate stageset <name> \
  stages.metio.wtf/approved-version=2.0.0 --overwrite
```

The controller proceeds on the next reconcile once the annotation equals the
desired version. Because approval is tied to that exact version, a later
transition to a different version holds again until you approve the new target —
a stale approval never carries over.
