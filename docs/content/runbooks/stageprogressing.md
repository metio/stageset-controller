---
title: StageProgressing
description: A stage's verify wait ran out of time while its objects were still coming up, so the run is holding rather than failing — it converges on its own.
tags: [runbooks, stages, timeout, troubleshooting]
---

## Symptom

`READY=False`, `REASON=StageProgressing`. The Message names the stage and what the wait was still waiting on:

```text
kubectl --namespace <ns> describe stageset <name>
...
Status:
  Conditions:
    Reason:  StageProgressing
    Status:  False
    Type:    Ready
    Message: stage "server" is applied and still becoming ready: timeout waiting for:
             [Deployment/<ns>/server status: 'InProgress'] (waited 15m: stage
             readyChecks.timeout, else stages[].timeout, else spec.timeout)
```

The stage's objects are applied and the stage shows `Verifying` in `status.stages`.

**This is not a failure, and it does not need you.** The next reconcile re-verifies, and the wait returns the moment the workload reports ready. Read on only if it is not converging.

## Cause

The verify wait fails fast: an object that reaches a terminal state — an image that cannot be pulled, a container crash-looping past its threshold — aborts the wait immediately and reports [`StageFailed`](/runbooks/stagefailed/). So the timeout is only ever reached by objects that were *still making progress* when the clock ran out.

That is almost always a workload that needs longer than its stage allows: a first install against an empty database, running migrations and seed data before it serves.

Because nothing failed, `spec.rollbackOnFailure` deliberately does **not** engage — restoring the previous manifests over a half-finished migration is worse than letting a slow rollout finish. See [`onTimeout`](/api/stageset/#scheduling) to change that.

## Diagnosis

Confirm the workload is actually progressing rather than wedged:

```shell
kubectl --namespace <ns> rollout status deployment/<name> --timeout=0
kubectl --namespace <ns> get pods --selector app=<name>
kubectl --namespace <ns> logs deployment/<name> --tail=50
```

A pod that is `Running` with no restarts and a startup probe still failing is mid-startup — the normal case here. A pod stuck `Pending` (unschedulable, unbound PVC) or restarting repeatedly is not progressing, and the stage will eventually be reported as `StageFailed` instead.

If the condition keeps reappearing on every reconcile, the workload takes longer than its stage timeout allows on **every** attempt, so the run never advances past it.

### What the log shows while a stage is waiting

A verify wait blocks for as long as the stage's timeout allows, so a slow stage means a long stretch with no state change to report. For every wait that outlasts a minute, the controller reports in once a minute:

```text
stage is still becoming ready; verify wait continues
  stage=server elapsed=6m0s timeout=15m0s deadline=2026-08-02T00:17:27Z
```

Rollouts that become ready inside the first minute log nothing extra, so these lines appear only where they explain something. Their absence, on a StageSet that has not reached `Ready`, means the controller is not in a verify wait — start from [controller pod down](/runbooks/controller-pod-down/) and [workqueue saturation](/runbooks/workqueue-saturation/) instead of raising the timeout.

### Deleting a StageSet that is waiting

Deletion is acted on within seconds: the wait is abandoned as soon as the `deletionTimestamp` lands, and reverse-order teardown starts without waiting out the rest of the timeout. A stage that never becomes ready is exactly the one you are most likely to tear down and retry, so it does not cost you the stage's timeout to do so.

## Remediation

Nothing, if the workload converges — the rollout resumes by itself and `Ready` goes true.

Where it repeats, raise the stage's budget so a single attempt can finish, most specific level first:

```yaml
spec:
  stages:
    - name: server
      readyChecks:
        timeout: 30m          # this stage's readiness only
      # timeout: 30m          # or: this whole stage
  # timeout: 30m              # or: every stage in the run
```

Make it at least as long as the workload's own declared startup budget — a `startupProbe`'s `failureThreshold × periodSeconds` plus `initialDelaySeconds` — so the two numbers agree instead of the shorter one winning.

The retry cadence between attempts is `spec.retryInterval` (falling back to `spec.interval`), so shorten that if you want the re-check sooner.

## Why this is not StageFailed

A portal or a fleet gate watching `Ready` has to tell a release that is still coming up from one that broke: the first converges unattended, the second needs someone. Reporting both as `StageFailed` made every slow first install look like an outage. The split is what makes "wait" and "page someone" different answers.
