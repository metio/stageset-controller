---
title: API reference
---

The controller owns two custom resources in the `stages.metio.wtf/v1` API group.
Every field is described with its type, whether it is required, its default, and
what it does.

- [`StageSet`](/api/stageset/) — the resource you author: an ordered set of
  stages, with scheduling, security, gating, versioning, and rollback.
- [`StageInventory`](/api/stageinventory/) — controller-managed; records the
  objects each stage applied so the controller can prune precisely.
