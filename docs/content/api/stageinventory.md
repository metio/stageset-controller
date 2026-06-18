---
title: StageInventory
description: The controller-managed inventory of objects each stage applied.
tags: [api, stages, crd, troubleshooting]
---

```yaml
apiVersion: stages.metio.wtf/v1
kind: StageInventory
```

A `StageInventory` records the set of objects a single stage has applied, so the
controller can prune precisely and tear stages down in reverse order. **You do not
author these** — the controller creates, updates, and deletes them. The fields
below let you read inventory state when debugging and back it up.

One stage may be backed by several `StageInventory` shards once it exceeds
`--inventory-shard-cap` entries (default 5000), so a single object never grows
unbounded.

## spec

A `StageInventory` as the controller writes it (read-only — never hand-author one):

```yaml
apiVersion: stages.metio.wtf/v1
kind: StageInventory
metadata:
  name: payments-application-0          # <stageset>-<stage>-<shard>
  namespace: payments
  labels:
    stages.metio.wtf/stage-set: payments
    stages.metio.wtf/stage: application
    stages.metio.wtf/shard: "0"
spec:
  stagePosition: 1                       # the stage's index in spec.stages
  entries:                               # identifiers only — never object contents
    - id: payments_web_apps_Deployment   # namespace_name_group_kind
      v: apps/v1                         # the applied API version
    - id: payments_web__Service          # empty group → core/v1
      v: v1
```

The inventory is stored in `spec` (not `status`) on purpose: backup tooling that
restores `spec` preserves the prune history, so a restored controller does not
orphan or wrongly prune objects.

| Field | Meaning |
|---|---|
| `stagePosition` | The stage's index in `spec.stages` at write time. Teardown walks inventories in reverse position order. |
| `entries[].id` | An applied object's identifier, form `namespace_name_group_kind` (empty group for core). |
| `entries[].v` | The API version the object was applied at. |

## Well-known labels and annotations

The controller stamps these onto inventories and managed objects:

| Key | On | Meaning |
|---|---|---|
| `stages.metio.wtf/stage-set` | inventory | Owning StageSet name. |
| `stages.metio.wtf/stage` | inventory | Stage name. |
| `stages.metio.wtf/shard` | inventory | Shard index. |
| `stages.metio.wtf/name` | managed object | Owning StageSet name — `kubectl get -l stages.metio.wtf/name=<stageset>`. |
| `stages.metio.wtf/namespace` | managed object | Owning StageSet namespace. |
| `stages.metio.wtf/stage` | managed object | Stage that applied the object — `kubectl get -l stages.metio.wtf/stage=<stage>` for per-stage discovery. |
| `stages.metio.wtf/prune` | managed object | Set to `disabled` to opt an object out of pruning. |

Other annotations the controller honors on a StageSet or its objects. Each has a
[`stagesetctl reconcile`](/cli/reconcile/) equivalent:

- `reconcile.fluxcd.io/requestedAt` — request an out-of-band reconcile.
- `stages.metio.wtf/reconcile-stage` — force a single stage to re-run.
- `stages.metio.wtf/update-now` — push a window-held rollout through immediately.
