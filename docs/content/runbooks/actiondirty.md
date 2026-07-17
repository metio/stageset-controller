---
title: ActionDirty
description: A stage with an incomplete once-per-lifetime action failed repeatedly, so the controller halted auto-retry; clear the halt with a manual reconcile once the cause is fixed.
tags: [runbooks, actions, troubleshooting]
---

## Symptom

`READY=False`, `REASON=ActionDirty`. The Message names the stage whose action
ladder keeps failing. The controller has **stopped auto-retrying** — there is no
backoff requeue — and `status.actionFailureCount` is at the threshold. The stage
carries a `scope: Lifetime` action that has not yet completed.

## Cause

A stage whose action ladder still has a `scope: Lifetime` action to complete
failed repeatedly (the consecutive-failure threshold). A `scope: Lifetime`
action is a once-ever bootstrap — a database install, a schema create, a seed —
and re-attempting it against an uncertain state can be destructive. Rather than
keep retrying, the controller halts and waits for a human, the same model as
[`MigrationDirty`](/runbooks/migrationdirty/).

A persistently-failing bootstrap usually means something needs fixing by hand: a
bad action, a half-applied change, missing RBAC on the stage's ServiceAccount, an
unreachable dependency, or — for an anchored action — a `completionAnchor` that
names an object the stage never applies (so its witness can never be read at
completion).

## Remediation

1. Diagnose the failure from the Message and the stage's Events; the earlier
   failures surfaced as `StageFailed`.
2. Fix the underlying cause. If a destructive bootstrap partially applied,
   reconcile the real-world state by hand so the action can complete. The
   StageLedger records only completions, so an action that never recorded will
   run again once the stage can reach it.
3. Clear the halt with a manual reconcile, which resets the failure count and
   re-attempts the stage once:

   ```shell
   kubectl annotate stageset <name> \
     reconcile.fluxcd.io/requestedAt="$(date +%s)" --overwrite
   ```

A reconcile that applies and verifies every stage also resets the count to zero.
