---
title: Ready checks
description: Gate a stage on health with kstatus and CEL expressions.
tags: [ready-checks, health, stages]
---

Ready checks decide when a stage is healthy enough to let the next stage start.
They are purely observational — the controller waits and reports, but takes no
action (active steps are [actions](/defining-a-release/actions/)).

By default, with no `readyChecks` block, the controller waits for **every** object
the stage applied to report ready via
[kstatus](https://github.com/kubernetes-sigs/cli-utils/tree/master/pkg/kstatus).
`readyChecks` lets you narrow that to specific objects (`checks`), add custom
health for resources kstatus doesn't understand (`exprs`, [CEL](https://github.com/google/cel-spec)),
bound the wait (`timeout`), or skip it entirely (`disableWait`). `checks` and
`exprs` may be set together.

## Explicit objects

Wait for named objects only — useful when a stage applies many objects but only a
few gate the next stage:

```yaml
spec:
  stages:
    - name: infrastructure
      sourceRef:
        name: platform
      readyChecks:
        timeout: 5m
        checks:
          - apiVersion: apiextensions.k8s.io/v1
            kind: CustomResourceDefinition
            name: ledgers.payments.example
          - apiVersion: apps/v1
            kind: Deployment
            name: ledger-operator
            namespace: platform-system
```

## Custom health with CEL

For custom resources kstatus doesn't understand, describe readiness with CEL
expressions. The shape matches `kustomize-controller`'s `healthCheckExprs`, so
expressions are portable.

```yaml
      readyChecks:
        exprs:
          - apiVersion: db.example/v1
            kind: Database
            current: "status.phase == 'Running'"
            inProgress: "status.phase in ['Pending', 'Provisioning']"
            failed: "status.phase == 'Failed'"
```

## Opting out

To apply a stage without waiting for readiness (fire-and-forget), disable the
wait:

```yaml
      readyChecks:
        disableWait: true
```
