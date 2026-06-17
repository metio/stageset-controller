---
title: UpdateDeferred
description: A new revision is held by a closed update window.
tags: [runbooks, update-windows, scheduling, troubleshooting]
---

## Symptom

`READY=False`, `REASON=UpdateDeferred` (initial deploy held), or `READY=True` with a message noting a deferral and a populated `status.pendingUpdate` (an already-deployed StageSet with a held update).

## Cause

This is **not a failure** — it is time-based delivery working as configured. A new revision (or the first deploy) is being held because the StageSet's [`spec.updateWindows`](/usage/update-windows/) do not currently permit a rollout: either a `Deny` window is active, or `Allow` windows are declared and none is active right now. With `spec.windowScope: All`, even drift correction is paused while a window is closed.

`status.pendingUpdate` shows the held revisions and `nextWindowOpens` (when delivery resumes); the controller requeues at that boundary.

## Diagnosis

```shell
kubectl --namespace <namespace> get stageset <name> --output jsonpath='{.status.pendingUpdate}'
kubectl --namespace <namespace> get stageset <name> --output jsonpath='{.spec.updateWindows}'
```

Confirm the current time (in each window's `timeZone`) against the windows. An already-deployed StageSet stays `Ready=True` — the deployed version keeps running while the update waits.

## Remediation

Usually none — the update applies automatically when the next window opens. If you need it sooner:

- **Force it through once** (e.g. an emergency fix during a freeze):

  ```shell
  kubectl --namespace <namespace> annotate --overwrite stageset <name> \
    stages.metio.wtf/update-now="$(date +%s)"
  ```

  This applies the held rollout immediately, regardless of windows (one-shot per annotation value).
- **Adjust the windows** if the schedule is wrong — check `type` (Allow vs Deny), the cron `schedule`/`duration` or absolute `from`/`to`, and especially the `timeZone`.
