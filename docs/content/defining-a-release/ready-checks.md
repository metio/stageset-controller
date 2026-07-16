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

Cluster-scoped kinds work here — the `CustomResourceDefinition` above is the
common one, and `ClusterRole`, `Namespace`, `PersistentVolume`, and
`StorageClass` behave the same. Leave `namespace` unset for them; a
cluster-scoped object has none, and the field is ignored if you set one anyway.
For a namespaced kind, `namespace` defaults to the StageSet's when omitted.

This is the gate behind the usual ordering: an early stage installs an operator
and its CRDs, a check on the CRD holds the rollout until the API is served, and a
later stage applies the custom resources that need it.

Scope is resolved against the cluster the stage targets, so a stage with
`spec.kubeConfig` is judged by the remote cluster's own API surface.

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
