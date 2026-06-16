---
title: Controller pod down
description: A stageset-controller pod has been NotReady for the alert window.
tags: [runbooks, operations, alerts, troubleshooting]
---

## Symptom

A `stageset-controller` pod is `NotReady`; the `StageSetControllerPodDown` alert
fires. While no replica is Ready, StageSets are not reconciled and the
[Kubernetes](https://kubernetes.io/docs/) admission webhook may reject `StageSet`
writes (`failurePolicy: Fail`).

## Cause

- a crash-looping container (bad config flag, missing RBAC, panic),
- the node draining or out of resources,
- a failing readiness probe (`/readyz` on `--health-probe-bind-address`),
- the leader-election lease unobtainable.

## Diagnosis

```shell
kubectl -n stageset-system get pods -l app.kubernetes.io/name=stageset-controller
kubectl -n stageset-system describe pod <pod>
kubectl -n stageset-system logs <pod> --previous --tail=200
```

Look for flag-parse errors at startup, RBAC `Forbidden` on the controller's own
`ServiceAccount`, or OOMKills.

## Remediation

- Fix the surfaced cause (correct the flag/values, grant the missing controller
  RBAC, raise resource limits).
- Run more than one replica with leader election so a single pod failure doesn't
  stop reconciliation — see [production](/installation/production/#high-availability).
- If admission is blocking writes during the outage and you must unblock urgently,
  scope or relax the webhook `failurePolicy`, then restore it once the controller
  is healthy.

See [operations](/installation/operations/) for the full alert set and its thresholds.
