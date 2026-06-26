---
title: Stage promotion
description: Hold a rollout at a stage with a soak window or a manual gate before it advances.
tags: [promotion, stages, scheduling]
---

A promotion gate decides whether a healthy, applied stage may *advance* the
rollout to the next stage. It is distinct from [`readyChecks`](/defining-a-release/ready-checks/),
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
`false`, and is distinct from a migration's [`requireApproval`](/gating/versioned-migrations/),
which gates a destructive version transition rather than a stage advance.

## Advance only if metrics stay healthy

An analysis advances the stage only while metric checks against an external
source keep passing — error rate, latency, an SLO burn rate, anything Prometheus
can answer. This sees behavior a Deployment's own `.status` cannot, and is the
difference between "did it become Ready" ([`readyChecks`](/defining-a-release/ready-checks/))
and "is it behaving" (analysis).

```yaml
    - name: staging
      sourceRef:
        name: web-staging
      promotion:
        soak: 10m            # observe across this window
        analysis:
          interval: 1m       # re-evaluate this often while holding; defaults to spec.interval
          failureLimit: 3    # consecutive failing evaluations tolerated; defaults to 0
          checks:
            - name: error-rate
              source:
                prometheus:
                  address: http://prometheus.monitoring:9090
                  query: sum(rate(http_requests_total{stage="staging",code=~"5.."}[1m]))/sum(rate(http_requests_total{stage="staging"}[1m]))
              threshold:
                max: "0.01"  # ≤1% 5xx
            - name: latency-p99
              source:
                prometheus:
                  address: http://prometheus.monitoring:9090
                  query: histogram_quantile(0.99, sum(rate(http_request_duration_seconds_bucket{stage="staging"}[5m])) by (le))
              threshold:
                max: "0.5"   # ≤500ms p99
```

Every check must stay within its [`threshold`](/gating/error-budget/) (`min`
and/or `max`, inclusive). A breach increments a failure counter; the counter
resets on a passing evaluation and fails the promotion once it exceeds
`failureLimit`. While analysis is holding, the StageSet reports `Ready=False` with
reason `Soaking` (during the soak) or, once a check has failed past the limit,
`PromotionBlocked`. Each check's last observed value and verdict are on
`status.stages[].promotionState.lastAnalysis`. The analysis shares the metric
source contract with the [error-budget freeze](/gating/error-budget/), including
its SSRF guard and `secretRef` bearer-token support.

`onFailure` decides what a failed analysis does:

- `Hold` (default) — leave the stage applied but not promoted, surfacing why.
- `Rollback` — revert this stage to its last-known-good revision (requires
  [`spec.rollbackOnFailure`](/gating/rollback/) so a snapshot exists) and park the
  failing revision so it isn't re-applied each reconcile. Scoped to this stage:
  earlier promoted stages are untouched.

`onSourceError` decides what an unreadable source does, defaulting to `Hold` —
never advance a stage whose behavior can't be verified. (This is the opposite of
the error-budget freeze's `Allow` default, because holding a promotion only parks
the rollout at the current healthy stage.) See the
[PromotionBlocked](/runbooks/promotionblocked/) and
[BudgetSourceUnavailable](/runbooks/budgetsourceunavailable/) runbooks. While
tuning a new analysis, set `dryRun: true` to record what *would* block without
holding or rolling back anything.

## Promote early when the system is healthy

A long soak is insurance against a slow regression — but when the system is
demonstrably healthy there's no reason to wait it out. `fastTrack` shortens a
soak based on a metric: once a minimum soak has elapsed and a burn-rate (or
similar) metric stays within bounds, the stage promotes early.

```yaml
      promotion:
        soak: 30m              # the maximum soak
        fastTrack:
          after: 5m            # always soak at least this long
          max: "1"             # promote early once burn rate <= 1
          source:
            prometheus:
              address: http://prometheus.monitoring:9090
              query: slo:current_burn_rate:ratio{sloth_service="checkout"}
```

`fastTrack` only ever promotes *earlier* than `soak` — it never extends it. If the
metric is over `max`, unreadable, or the minimum `after` hasn't elapsed, the stage
just keeps soaking as normal. Use it with `analysis` when you also want to *block*
on a bad metric: analysis holds/rolls back on breach, fastTrack accelerates on
health. `fastTrack` requires a `soak` (there's nothing to shorten otherwise), and
`after` defaults to `0` (a healthy metric can promote immediately).

## Combine soak and a manual gate

With both set, the stage soaks first, then awaits a manual promotion:

```yaml
      promotion:
        soak: 30m
        requireManualPromotion: true
```

`stagesetctl promote` is also a break-glass: promoting a soaking stage ends the
soak early and advances it immediately, whichever gate is currently holding it.
