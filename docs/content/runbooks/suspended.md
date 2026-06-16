---
title: Suspended
description: Reconciliation is paused via spec.suspend.
tags: [runbooks, operations, troubleshooting]
---

## Symptom

`READY=False`, `REASON=Suspended`.

## Cause

`spec.suspend: true` is set, so the controller short-circuits before any resolution, build, or apply. This is an intentional operator action, not a failure — applied objects are left exactly as they were at the last successful run.

## Remediation

Resume by clearing the flag:

```shell
kubectl patch stageset <name> -n <namespace> --type=merge -p '{"spec":{"suspend":false}}'
```

The next reconcile picks up from the current artifact revisions.
