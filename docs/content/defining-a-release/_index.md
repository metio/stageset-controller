---
title: Defining a release
description: The core StageSet spec — ordered stages and their sources, typed actions, ready checks, conflict policies, and the inventory that tracks what was applied.
tags: [stages, configuration, sources]
---

These pages cover what goes in a StageSet's `spec`: the ordered stages, where
each one reads from, and how each stage applies, verifies, and is cleaned up.
Each page has a complete, copy-pasteable example; the
[API reference](/api/stageset/) has the exhaustive field-by-field detail.

- **[Stages and sources](/defining-a-release/stages-and-sources/)** — the ordered
  stages and the Flux source each one applies.
- **[Actions](/defining-a-release/actions/)** — typed pre, post, and on-failure
  steps around a stage's apply.
- **[Ready checks](/defining-a-release/ready-checks/)** — hold a stage until its
  objects report healthy.
- **[Conflict policies](/defining-a-release/conflict-policies/)** — what to do
  when an apply hits an immutable-field conflict.
- **[Inventory and pruning](/defining-a-release/inventory/)** — how the
  controller tracks and removes what each stage applied.
</content>
