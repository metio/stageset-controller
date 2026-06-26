---
title: PromotionBlocked
description: A stage's promotion analysis breached its thresholds, so the rollout will not advance past it.
tags: [runbooks, promotion, analysis, metrics, troubleshooting]
---

## Symptom

`READY=False`, `REASON=PromotionBlocked`. `status.stages[].promotionState` shows `phase: Blocked`, an `analysisFailures` count, and `lastAnalysis` with each check's observed value and verdict. When the stage's `onFailure` is `Rollback`, a Warning event records the revert and `promotionState.abortedRevision` names the revision that was rolled back.

## Cause

A [promotion analysis](/usage/stage-promotion/) on the named stage failed: one or more checks read a metric outside its threshold for more than `failureLimit` consecutive evaluations. The stage applied and became Ready, but its *observed behavior* — error rate, latency, SLO burn, whatever the checks measure — is out of bounds, so the rollout is not advanced to the next stage.

`onFailure` decides what happened:

- `Hold` (default): the stage stays applied but not promoted, surfacing why.
- `Rollback`: the stage was reverted to its last-known-good revision (scoped to this stage; earlier promoted stages are untouched) and the failing revision is parked so it is not re-applied each reconcile.

## Diagnosis

```shell
kubectl --namespace <namespace> get stageset <name> --output jsonpath='{.status.stages[*].promotionState.lastAnalysis}'
kubectl --namespace <namespace> get stageset <name> --output jsonpath='{.spec.stages[*].promotion.analysis}'
```

`lastAnalysis.checks[]` shows each check's observed `value`, whether it was `ok`, and any source `error`. Confirm whether the metric reflects a real regression in the new revision or a too-tight threshold / wrong query. The `stageset_stage_promotion_blocked` gauge is `1` for a blocked stage.

## Remediation

If the new revision is genuinely bad, fix it forward (push a corrected revision) — a new revision clears the failure count and re-runs the analysis. If the analysis itself is wrong (threshold too tight, query off), correct `spec.stages[].promotion.analysis` and re-reconcile. To accept the current revision despite the analysis (you have confirmed it is healthy), promote past the gate:

```shell
stagesetctl promote <name> --namespace <namespace> --stage <stage>
```

While tuning a new analysis, set `dryRun: true` on it so it records what *would* block without holding or rolling back anything.
