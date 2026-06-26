---
title: Soaking
description: A stage is applied and healthy and the rollout is holding through its promotion soak window before advancing.
tags: [runbooks, promotion, stages, scheduling, troubleshooting]
---

## Symptom

`READY=False`, `REASON=Soaking`. The Message names the stage and when the soak window closes. `status.stages[].promotionState` shows `phase: Soaking` with a `soakUntil` timestamp.

## Cause

This is **not a failure** — it is a promotion gate working as configured. The named stage applied cleanly and became Ready, and its [`spec.stages[].promotion.soak`](/gating/stage-promotion/) holds the rollout at this stage for the configured duration before advancing to the next one. A soak catches delayed regressions — an OOM after warm-up, error-rate creep, a crashloop after several minutes — that the point-in-time `readyChecks` cannot see.

The soak gates only *advancement to the next stage*. The soaking stage stays applied and its drift keeps being corrected; earlier stages remain promoted. The controller requeues at `soakUntil` and advances on its own once the window closes (provided the stage is still healthy).

## Diagnosis

```shell
kubectl --namespace <namespace> get stageset <name> --output jsonpath='{.status.stages}'
kubectl --namespace <namespace> get stageset <name> --output jsonpath='{.spec.stages[*].promotion}'
```

Compare `promotionState.soakUntil` against the current time. A new revision restarts the soak from the start, so a fresh rollout resets the clock.

## Remediation

Usually none — the rollout advances automatically when the soak elapses. If you need it sooner (for example to ship a fix without waiting out a long soak):

```shell
stagesetctl promote <name> --namespace <namespace> --stage <stage>
```

A manual promotion advances the stage immediately, ending the soak early. If a soak is consistently too long for the environment, lower or remove `spec.stages[].promotion.soak` for that stage — soaks are a per-stage trade-off (long for prod, short or absent for dev).
