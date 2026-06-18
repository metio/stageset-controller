---
title: API reference
---

The controller serves two custom resources in the `stages.metio.wtf/v1` API
group.

- [`StageSet`](/api/stageset/) — the resource you author: an ordered set of
  stages, with scheduling, security, gating, versioning, and rollback.
- [`StageInventory`](/api/stageinventory/) — controller-managed; it records the
  objects each stage applied so the controller can prune precisely.
