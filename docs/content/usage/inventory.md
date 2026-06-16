---
title: Inventory and pruning
description: How a StageSet records what each stage applied — the sharded, ApplySet-compliant StageInventory — and what the inventory modes change.
tags: [inventory, applyset, pruning, stages]
---

Every time a stage applies, the controller records the exact set of objects it
applied, so it can **prune** what a later revision no longer renders and tear a
StageSet down in reverse stage order. That record is the **`StageInventory`** — a
controller-owned resource you never author (its fields are in the
[API reference](/api/stageinventory/)). This page covers what the inventory is for
and the knob that changes how it is tracked: `--inventory-mode`.

## What the inventory records

For each stage, the controller writes a `StageInventory` listing every object the
stage owns — group, kind, namespace, name. On the next reconcile it diffs the new
render against that record and prunes the objects that dropped out, honoring
`prune: false` and the [conflict policy](/usage/conflict-policies/). On deletion it
walks the stages in reverse, so a later stage's objects are removed before the
earlier stages they depend on.

Large stages are **sharded**: once a stage's inventory passes `--inventory-shard-cap`
entries (default `5000`), it is split across multiple `StageInventory` objects so a
single object never grows unbounded. Sharding is automatic; the cap is a tuning
knob you rarely touch.

## Inventory modes

`--inventory-mode` (chart value `controller.inventoryMode`) selects how stage
membership is tracked. It is a controller-wide setting, echoed on each StageSet's
`status.inventoryMode`:

| Mode | What it writes | Reach for it when |
|---|---|---|
| `hybrid` (default) | The `StageInventory` entries **and** [ApplySet](https://kubernetes.io/docs/reference/labels-annotations-taints/#applyset-kubernetes-io-part-of) (KEP-3659) labels on every applied object | You want both the controller's pruning and `kubectl`/ApplySet tooling to see the set — the safe default |
| `entries` | Only the `StageInventory` entries — no labels on applied objects | You want the smallest on-object footprint and need no external ApplySet tooling |
| `applyset` | ApplySet labels on members plus the KEP-3659 parent metadata | You want the set to be a first-class, `kubectl`-discoverable ApplySet |

In `hybrid` and `applyset` modes the controller stamps each member with
`applyset.kubernetes.io/part-of` and marks the `StageInventory` parent with the
ApplySet `id`, the managing tooling, and the contains-group-kinds hint — so
`kubectl get --applyset` and other ApplySet-aware tools can list and prune the set.
`entries` mode skips those labels and relies only on the recorded entry list.

### Implications

- **Pruning is equally correct in every mode** — the controller always prunes from
  its own `StageInventory` record. The mode only changes whether applied objects
  *also* carry ApplySet labels.
- **`entries`** keeps applied objects free of extra labels — useful when another
  tool owns the ApplySet labels, or you want the minimal footprint.
- **`applyset` / `hybrid`** make the set interoperable with `kubectl apply --prune
  --applyset` and other KEP-3659 tooling, at the cost of one label per object.
- A mode change takes effect on the **next** apply of each stage; objects keep the
  labels they were last stamped with until then.

Set it once on the controller — it is a process-wide flag, not per-StageSet:

```yaml
# Helm values
controller:
  inventoryMode: hybrid        # entries | hybrid | applyset
  inventoryShardCap: 5000      # maximum entries per StageInventory shard
```

The [configuration reference](/installation/configuration/) lists both flags, and
the [StageInventory API](/api/stageinventory/) documents the object's fields.
