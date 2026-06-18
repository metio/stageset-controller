---
title: Progressive delivery
description: Gate a Flagger or Argo Rollouts promotion on a StageSet stage, and gate a StageSet stage on a Rollout.
tags: [tutorials, progressive-delivery, flagger, argo, stages]
---

`StageSet` integrates with both progressive-delivery controllers:
[Flagger](https://flagger.app/) and
[Argo Rollouts](https://argoproj.github.io/argo-rollouts/). The controller exposes
a read-only gate endpoint and a readiness gauge so either one can hold a promotion
until a `StageSet` stage is healthy; ready checks let a stage wait on a Rollout in
return. See also [StageSet vs Argo Rollouts](/comparisons/argo-rollouts/).

## The gate contract

The gate endpoint backs the Flagger integration and the Argo Rollouts JSON-metric
option.

```text
GET /gate/{namespace}/{stageset}/{stage}
  200  — the stage is Ready at the currently pinned revision
  403  — the stage is not Ready (or not found / not gateable)
```

It is served on `--gate-bind-address` (default `:8082`) and exposed by the chart's
`stageset-controller-gate` Service (`gate.enabled`, on by default). The endpoint is
**unauthenticated and read-only**, so fence it with a `NetworkPolicy`
([production](/installation/production/#network-policy)) to admit only your
delivery controller.

## Flagger

Add a `confirm-promotion` (or `confirm-rollout`) webhook to a Flagger `Canary`
pointing at the gate. Flagger blocks the promotion until the gate returns `200`:

```yaml
apiVersion: flagger.app/v1beta1
kind: Canary
metadata:
  name: web
  namespace: apps
spec:
  targetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: web
  analysis:
    interval: 1m
    threshold: 5
    stepWeight: 10
    maxWeight: 50
    webhooks:
      - name: stageset-stage-ready
        type: confirm-promotion
        # gate this canary's promotion on a StageSet stage being Ready
        url: http://stageset-controller-gate.stageset-system:8082/gate/apps/web/web
```

This is independent of the Flagger *strategy*: the same webhook gates a weighted
**canary**, an **A/B test** (header/cookie routing), or a **blue-green** promotion
— the gate only answers "is this stage Ready," and Flagger decides what to do with
that answer.

This coordinates two moving parts: Flagger shifts traffic to a new version only once
a StageSet stage that applied the supporting config (a CRD, a migration, a sibling
component) reports Ready.

## Argo Rollouts

Argo Rollouts gates on **analysis metrics** (a query that returns a value to
compare) rather than a webhook's HTTP status, so the controller meets it on its own
terms in two ways.

### Gate on the readiness gauge (recommended)

The controller exports `stageset_stage_ready{namespace,stageset,stage}` (`1` when
the stage is Ready, `0` otherwise). Argo's **Prometheus** metric provider gates on
it directly — no gate endpoint, no Job:

```yaml
apiVersion: argoproj.io/v1alpha1
kind: AnalysisTemplate
metadata:
  name: stageset-stage-ready
  namespace: apps
spec:
  metrics:
    - name: stage-ready
      successCondition: result == 1
      provider:
        prometheus:
          address: http://prometheus.monitoring:9090
          query: max(stageset_stage_ready{namespace="apps",stageset="web",stage="web"})
```

### Gate on the JSON endpoint

The same gate endpoint also answers JSON when asked
(`Accept: application/json`), returning `{"ready": true, …}` with a `200` so Argo's
**web** metric can parse it (Argo treats a non-2xx as an error, so readiness has to
live in the body):

```yaml
spec:
  metrics:
    - name: stage-ready
      successCondition: "result.ready == true"
      provider:
        web:
          url: http://stageset-controller-gate.stageset-system:8082/gate/apps/web/web
          headers:
            - key: Accept
              value: application/json
          jsonPath: "{$}"
```

A **Job-based metric** (`curl -fsS …` against the gate, succeeding only on `200`)
is the fallback when the analysis has no Prometheus or web access.

## The reverse direction: gate a StageSet on a Rollout

The coordination also works the other way. Because
[ready checks](/usage/ready-checks/) accept CEL, a StageSet stage can wait on an
Argo `Rollout` finishing its own progressive rollout before the next stage runs:

```yaml
readyChecks:
  exprs:
    - apiVersion: argoproj.io/v1alpha1
      kind: Rollout
      current: "status.phase == 'Healthy'"
      inProgress: "status.phase in ['Progressing', 'Paused']"
      failed: "status.phase == 'Degraded'"
```

So StageSet can gate Argo (via the gauge/gate) and Argo's outcome can gate
StageSet (via ready checks) — pick whichever direction your release needs.
