---
title: Conflict policies
description: Resolve immutable-field and ownership conflicts per resource.
tags: [conflict-policies, stages]
---

Conflict policies decide what happens when an apply hits an immutable-field
conflict — a changed `clusterIP`, a `Job` pod template, a `StorageClass` field
that can't be updated in place. By default the controller fails the stage and
reports it, so nothing destructive happens by surprise. A policy opts specific
resources into automatic resolution.

## The three actions

- `Fail` — stop and report (the default; safest).
- `Recreate` — delete and re-create the object to get past an immutable-field
  change.
- `KeepExisting` — leave the live object as-is and move on.

## A default for the whole stage

```yaml
spec:
  stages:
    - name: app
      sourceRef:
        name: my-app
      conflictPolicy:
        default: Fail            # explicit; the safe default
```

The `force: true` shorthand on a stage is equivalent to
`conflictPolicy.default: Recreate`.

## Per-resource rules

Rules recreate exactly the resources that need it while everything else stays
`Fail`. A rule's `target` is a partial selector — any field you omit matches
everything. Rules are evaluated in list order; the **first** rule whose target
matches wins, and an object matching no rule falls back to `default`.

```yaml
      conflictPolicy:
        default: Fail
        rules:
          # a Job's pod template is immutable — recreate it on change
          - target:
              apiVersion: batch/v1
              kind: Job
            action: Recreate
          # never fight an HPA over replica counts
          - target:
              kind: Deployment
              name: web
            action: KeepExisting
```

## Recreating storage

Recreating a `PersistentVolumeClaim` or `PersistentVolume` destroys data, so a
`Recreate` **rule** targeting one is refused unless you explicitly accept the loss:

```yaml
        rules:
          - target:
              kind: PersistentVolumeClaim
              name: scratch
            action: Recreate
            allowDataLoss: true     # required for PVC/PV Recreate, refused otherwise
```

Without `allowDataLoss: true`, a `Recreate` rule targeting a PVC/PV is rejected —
a guardrail against accidentally wiping a volume.
