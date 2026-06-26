---
title: Inventory and pruning
description: How a StageSet records what each stage applied — the sharded StageInventory — prunes what a revision no longer renders, and labels every applied object for per-stage discovery.
tags: [inventory, pruning, stages]
---

Every time a stage applies, the controller records the exact set of objects it
applied, so it can **prune** what a later revision no longer renders and tear a
StageSet down in reverse stage order. That record is the **`StageInventory`** — a
controller-owned resource you never author (its fields are in the
[API reference](/api/stageinventory/)).

## What the inventory records

For each stage, the controller writes a `StageInventory` listing every object the
stage owns — group, kind, namespace, name. On the next reconcile it diffs the new
render against that record and prunes the objects that dropped out, honoring
`prune: false` and the [conflict policy](/defining-a-release/conflict-policies/). On deletion it
walks the stages in reverse, so a later stage's objects are removed before the
earlier stages they depend on.

Large stages are **sharded**: once a stage's inventory passes `--inventory-shard-cap`
entries (default `5000`), it is split across multiple `StageInventory` objects so a
single object never grows unbounded. Sharding is automatic; the cap is a tuning
knob you rarely touch.

## Discovering a stage's objects

Every applied object carries labels that let you find it without reading the
`StageInventory`:

- `stages.metio.wtf/name` — the owning StageSet.
- `stages.metio.wtf/namespace` — the StageSet's namespace.
- `stages.metio.wtf/stage` — the stage that applied the object.

So a single label selector enumerates exactly one stage's objects:

```shell
kubectl get all --namespace prod --selector stages.metio.wtf/stage=canary
```

These labels are for discovery only; pruning and teardown are driven by the
`StageInventory` record, which is authoritative and scope-agnostic (it tracks
cluster-scoped and namespaced objects alike). The controller owns pruning — there
is no `kubectl apply --prune` handoff to keep in sync.

```yaml
# Helm values
controller:
  inventoryShardCap: 5000      # maximum entries per StageInventory shard
```

The [configuration reference](/reference/configuration/) lists the flag, and
the [StageInventory API](/api/stageinventory/) documents the object's fields.
