---
title: AwaitingApproval
description: A version transition is held for approval; approve the target version to proceed.
tags: [runbooks, migrations, approvals, fleet]
---

## Symptom

`READY=False`, `REASON=AwaitingApproval`. The Message names the target version (and
the pending migration count, when the transition carries migrations).
`status.pendingMigrations` lists exactly what will run. Nothing is applied and
`status.version` is not advanced — this is an intentional hold, not an error.

## Cause

`spec.version.approvalMode` holds this transition until the target version is
approved:

- `OnMigrations` holds only a transition that carries migrations, so destructive
  migrations don't run unattended while a config-only version bump proceeds.
- `Always` holds every version advance — a fleet-managed StageSet sets this so a
  [`FleetRollout`](/gating/versioned-migrations/) can pace its adoption wave by wave.

The whole rollout (app and migrations) waits until the target version is authorized.

## Remediation

Review `status.pendingMigrations` (or `stagesetctl diff` / `plan`) to see what will
run, then approve the target version by setting the annotation to it:

```shell
kubectl annotate stageset <name> \
  stages.metio.wtf/approved-version=2.0.0 --overwrite
```

For a fleet-managed StageSet the `FleetRollout` controller stamps this annotation
when the StageSet's wave opens; approving by hand is a break-glass override.

The controller proceeds on the next reconcile once the annotation equals the
desired version. Because approval is tied to that exact version, a later transition
to a different version holds again until you approve the new target — a stale
approval never carries over.
