---
title: Stage promotion
description: Hold a rollout at a stage with a soak window or a manual gate before it advances.
tags: [promotion, stages, scheduling]
---

A promotion gate decides whether a healthy, applied stage may *advance* the
rollout to the next stage. It is distinct from [`readyChecks`](/usage/ready-checks/),
which decide whether a stage's just-applied objects became Ready: a promotion
gate runs *after* the stage is Ready and gates only advancement. Holding a gate
parks the rollout at the stage — the stage stays applied and its drift keeps
being corrected, and later stages are never touched.

Two mechanisms are available, and they compose: a `soak` window runs first, then
a manual gate.

## Soak before advancing

Hold at a stage for a fixed duration after it becomes Ready, advancing only if it
stays healthy for the whole window. This catches delayed regressions — an OOM
after warm-up, error-rate creep, a crashloop after several minutes — that
point-in-time `readyChecks` cannot see.

```yaml
apiVersion: stages.metio.wtf/v1
kind: StageSet
metadata:
  name: web
  namespace: apps
spec:
  serviceAccountName: deployer
  stages:
    - name: staging
      sourceRef:
        name: web-staging
      promotion:
        soak: 10m
    - name: prod
      sourceRef:
        name: web-prod
```

While `staging` soaks, the StageSet reports `Ready=False` with reason `Soaking`
and `status.stages[].promotionState` carries the `soakUntil` instant. The
controller requeues at that instant and advances on its own once the window
elapses. There is no default soak — omit `soak` (or set it to `0`) for no hold;
soaks are a per-stage trade-off, so use a long window for prod and none for dev.

## Require a manual promotion

Hold at a stage until an operator promotes it — the "confirm before prod" gate.

```yaml
    - name: prod
      sourceRef:
        name: web-prod
      promotion:
        requireManualPromotion: true
```

While held, the StageSet reports `Ready=False` with reason `AwaitingPromotion`.
Promote the stage when you are satisfied it is healthy:

```shell
stagesetctl promote web --namespace apps --stage prod
```

`promote` stamps a one-shot `stages.metio.wtf/promote` annotation that advances
the named stage exactly once; the rollout then continues. Add `--wait` to block
until the controller records the promotion. A manual gate that is always
rubber-stamped is theatre — it earns its keep when the operator actually checks
the stage's behavior before promoting. `requireManualPromotion` defaults to
`false`, and is distinct from a migration's [`requireApproval`](/usage/versioned-migrations/),
which gates a destructive version transition rather than a stage advance.

## Combine soak and a manual gate

With both set, the stage soaks first, then awaits a manual promotion:

```yaml
      promotion:
        soak: 30m
        requireManualPromotion: true
```

`stagesetctl promote` is also a break-glass: promoting a soaking stage ends the
soak early and advances it immediately, whichever gate is currently holding it.
