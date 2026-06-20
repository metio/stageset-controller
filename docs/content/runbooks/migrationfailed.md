---
title: MigrationFailed
description: A migration's action failed; the run halts at that stage and retries with backoff, skipping already-completed actions.
tags: [runbooks, migrations, actions, troubleshooting]
---

## Symptom

`READY=False`, `REASON=MigrationFailed`. The Message names the migration and the
failed action. The run halts at the anchoring stage; `status.version` is not
advanced. The controller retries with backoff.

## Cause

A migration anchored before a stage ran an action that failed (a `job` that
errored, an `http` call that returned an error, a `wait` that timed out, an
`apply`/`patch`/`delete` the apiserver rejected). This reason is distinct from
`StageFailed` so you can tell a migration failure from the stage's own apply.

Retries are safe for the actions that already completed: the per-action ledger
(`status.executedMigrationActions`, keyed by migration content digest) records
each finished action, so a retry resumes at the failed action rather than
re-running destructive work from the top.

## Fix

- Read the Message and the migration's Events (`MigrationStarted` /
  `MigrationFailed`) to find the failed action and its error.
- Fix the underlying cause (the target object, RBAC for the action, the endpoint
  an `http` action calls, the image a `job` runs).
- The migration re-attempts automatically on the next retry. After repeated
  failures it escalates to [`MigrationDirty`](../migrationdirty/), which halts
  auto-retry — see that page for the manual-recovery step.
