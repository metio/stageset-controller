---
title: Stalled
description: The run cannot make progress and will not retry until the spec changes.
tags: [runbooks, stages, troubleshooting]
---

## Symptom

`READY=False`, `REASON=Stalled`. Terminal: the controller does not requeue until the spec changes.

## Cause

A condition that retrying cannot clear. Currently this is a **`spec.dependsOn` cycle** — two or more StageSets depend on each other (directly or transitively), so none can ever become Ready first. The cycle is detected by a breadth-first walk over the `dependsOn` graph. A dependency that is merely not Ready yet (no cycle) reports [`DependencyNotReady`](/runbooks/dependencynotready/) instead.

## Diagnosis

```shell
kubectl describe stageset <name> -n <namespace>     # Message states "spec.dependsOn forms a cycle"
# Trace the edges:
kubectl get stageset -n <namespace> \
  -o custom-columns=NAME:.metadata.name,DEPENDSON:.spec.dependsOn[*].name
```

Follow the `dependsOn` names until you find the loop (A → B → A, or longer).

## Remediation

Break the cycle by removing one edge — drop a `dependsOn` entry from one StageSet, or restructure so the ordering is a strict chain. Dependencies must form a directed acyclic graph. After the edit, the next reconcile re-walks the graph and clears the condition.
