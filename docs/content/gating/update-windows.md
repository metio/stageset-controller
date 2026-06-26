---
title: Update windows
description: Allow or deny rollouts on a schedule, without pausing reconciliation.
tags: [update-windows, scheduling, stages]
---

Update windows gate *when* new artifact revisions roll out, without pausing
reconciliation. Drift correction keeps running; only the rollout of a *new*
revision is held until a window allows it.

## Deny a recurring window

Freeze rollouts during business hours:

```yaml
spec:
  stages:
    - name: app
      sourceRef:
        name: my-app
  updateWindows:
    - type: Deny
      schedule: "0 9 * * MON-FRI"   # 5-field cron: start of the window
      duration: 8h
      timeZone: Europe/Berlin
```

A new revision that arrives inside the window is held; `status.pendingUpdate`
records what is waiting and `nextWindowOpens` when it will ship. The controller
emits an `UpdateDeferred` event and increments `stageset_update_deferred_total`.

## Allow-list windows

If any `Allow` window exists, rollouts happen **only** inside an active Allow with
no active Deny — `Deny` always wins. This expresses "only deploy on Tuesday and
Thursday afternoons":

```yaml
  updateWindows:
    - type: Allow
      schedule: "0 14 * * TUE,THU"
      duration: 3h
      timeZone: America/New_York
```

## A one-off freeze

Absolute windows use `from`/`to` instead of a schedule — for a planned event
freeze:

```yaml
  updateWindows:
    - type: Deny
      from: 2026-12-24T00:00:00Z
      to:   2026-12-27T00:00:00Z
```

## What a closed window blocks

`windowScope` controls what a closed window holds back:

- **`Updates`** (default) — hold only the rollout of a *new* artifact revision.
  Drift correction keeps re-applying the pinned state, so the live cluster stays
  on its last-approved revision but doesn't fall out of sync.
- **`All`** — a hard freeze: also pause drift correction, so the controller
  applies nothing at all while the window is closed.

```yaml
  windowScope: Updates   # default: hold new revisions, keep correcting drift
  # windowScope: All     # hard freeze: also pause drift correction
```

## Shipping anyway

To push a held rollout through immediately, override the window with
[`stagesetctl`](/cli/):

```shell
stagesetctl reconcile my-app --update-now
```

This stamps the `stages.metio.wtf/update-now` annotation; the honored value is
recorded in `status.lastHandledUpdateOverride`.

## Related gates

An update window gates *when* a new revision starts rolling out. Two other gates
control *whether* a rollout proceeds, and combine with it:

- **[Stage promotion](/gating/stage-promotion/)** — hold a stage with a soak
  window or a manual gate before it advances.
- **[Error-budget freeze](/gating/error-budget/)** — pause new-revision rollouts
  while a service is out of its SLO error budget.
