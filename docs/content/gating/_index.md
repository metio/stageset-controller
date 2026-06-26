---
title: Gating & rollout safety
description: Control when and whether a rollout proceeds — update windows, promotion gates, error-budget freezes, versioned migrations, and rollback.
tags: [gating, scheduling, rollback]
---

A stage can be healthy and still not the right moment to advance. These features
decide *when* a new revision rolls out and *whether* it proceeds, and undo it
when it goes wrong — they compose, so you can require an open window, a healthy
budget, and a passed soak all at once.

- **[Update windows](/gating/update-windows/)** — allow or deny new revisions on a
  schedule.
- **[Stage promotion](/gating/stage-promotion/)** — hold a stage with a soak
  window, a manual gate, or a metric analysis before it advances.
- **[Error-budget freeze](/gating/error-budget/)** — pause rollouts while a
  service is out of its SLO error budget.
- **[Versioned migrations](/gating/versioned-migrations/)** — run migrations when
  a release crosses a version boundary.
- **[Rollback](/gating/rollback/)** — restore the last-good revision when a
  rollout fails.
</content>
