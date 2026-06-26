---
title: AwaitingPromotion
description: A stage is applied and healthy and the rollout is holding for a manual promotion.
tags: [runbooks, promotion, stages, troubleshooting]
---

## Symptom

`READY=False`, `REASON=AwaitingPromotion`. The Message names the stage waiting to be promoted. `status.stages[].promotionState` shows `phase: AwaitingManual`.

## Cause

This is **not a failure** — it is a promotion gate working as configured. The named stage applied cleanly and became Ready, and its [`spec.stages[].promotion.requireManualPromotion`](/gating/stage-promotion/) holds the rollout there until an operator promotes it. This is the "hold before prod" gate: a human confirms the stage is good (ideally after reviewing metrics) before the rollout advances.

The gate holds only *advancement to the next stage*. The awaiting stage stays applied and its drift keeps being corrected; earlier stages remain promoted. A manual promotion gate is distinct from a migration's `requireApproval`, which gates a destructive version transition rather than a stage advance.

## Diagnosis

```shell
kubectl --namespace <namespace> get stageset <name> --output jsonpath='{.status.stages}'
```

The stage whose `promotionState.phase` is `AwaitingManual` is the one waiting. Verify it is behaving as expected before promoting.

## Remediation

Promote the stage when you are satisfied it is healthy:

```shell
stagesetctl promote <name> --namespace <namespace> --stage <stage>
```

This stamps a one-shot `stages.metio.wtf/promote` annotation that advances the named stage exactly once; the rollout then continues to the next stage. To promote without the CLI:

```shell
kubectl --namespace <namespace> annotate --overwrite stageset <name> \
  stages.metio.wtf/promote="<stage>@$(date +%s)"
```

A manual gate that is always rubber-stamped is theatre — it earns its keep when the operator actually checks the stage's behavior (in a later release, paired with promotion analysis) before promoting.
