---
title: DependencyNotReady
description: A StageSet named in spec.dependsOn is not yet Ready.
tags: [runbooks, stages, troubleshooting]
---

## Symptom

`READY=False`, `REASON=DependencyNotReady`. Transient: the controller requeues at `spec.retryInterval` (or `spec.interval`).

## Cause

A StageSet listed in `spec.dependsOn` is not `Ready` at its observed generation, so this StageSet holds before doing any work. Semantics match kustomize-controller: a dependency is satisfied only when its `Ready=True` **and** its `status.observedGeneration` equals its current generation (so a freshly-edited dependency mid-reconcile does not count as ready).

## Diagnosis

```shell
kubectl describe stageset <name> -n <namespace>            # Message names the dependency
kubectl get stageset <dependency> -n <namespace>           # is it Ready?
kubectl describe stageset <dependency> -n <namespace>      # why not?
```

## Remediation

Resolve the dependency's own Ready condition first (follow its runbook). Once it reports `Ready=True` at its current generation, this StageSet proceeds on the next reconcile. If the dependency is intentionally [suspended](/runbooks/suspended/), this StageSet waits indefinitely by design — remove the `dependsOn` entry or resume the dependency.

A `dependsOn` **cycle** is reported as [`Stalled`](/runbooks/stalled/), not this reason.
