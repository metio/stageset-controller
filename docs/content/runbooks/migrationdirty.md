---
title: MigrationDirty
description: A migration failed repeatedly, so the controller halted auto-retry; clear the halt with a manual reconcile once the cause is fixed.
tags: [runbooks, migrations, troubleshooting]
---

## Symptom

`READY=False`, `REASON=MigrationDirty`. The Message names the migration that keeps
failing. The controller has **stopped auto-retrying** — there is no backoff
requeue — and `status.version` is not advanced. `status.migrationFailureCount` is
at the threshold.

## Cause

A migration failed repeatedly (the consecutive-failure threshold). Rather than
keep re-attempting destructive work against an uncertain state, the controller
halts and waits for a human — the golang-migrate "dirty" / Flyway "repair" model.
A persistently-failing destructive migration usually means something needs
fixing by hand (a bad action, a half-applied change, missing RBAC, a broken
target) that retrying cannot resolve.

## Fix

1. Diagnose the failure from the Message and the migration's Events; the earlier
   failures surfaced as [`MigrationFailed`](../migrationfailed/).
2. Fix the underlying cause. If a destructive action partially applied, reconcile
   the real-world state by hand so the migration's actions can complete (the
   per-action ledger will skip the actions that already succeeded).
3. Clear the halt with a manual reconcile, which resets the failure count and
   re-attempts the migration once:

   ```shell
   flux reconcile source ... && kubectl annotate stageset <name> \
     reconcile.fluxcd.io/requestedAt="$(date +%s)" --overwrite
   ```

The transition also resets the count to zero once it completes successfully.
