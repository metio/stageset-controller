---
title: StageFailed
description: A stage failed to fetch, build, apply, verify, or run an action; the run halts there.
tags: [runbooks, stages, actions, troubleshooting]
---

## Symptom

`READY=False`, `REASON=StageFailed`. The Message names the stage and the operation that failed (`fetch artifact`, `build`, `apply`, `verify`, a pre/post action, or `connect to target cluster`). The run halts at that stage; later stages keep their previous revisions.

## Cause

A stage failed during execution. By operation:

- **fetch artifact** — the artifact URL was unreachable, or its bytes failed digest verification.
- **build** — kustomize build or post-build substitution failed (a missing `substituteFrom` source, an invalid patch, a malformed manifest).
- **apply** — the server-side apply was rejected: an immutable-field conflict, or an **RBAC denial** under the impersonated `serviceAccountName`.
- **verify** — applied objects did not become Ready within the stage timeout (kstatus).
- **pre/post action** — a `patch`/`http`/`wait`/`job`/`delete`/`apply` action failed or timed out.
- **connect to target cluster** — a `spec.kubeConfig` Secret was missing, unparseable, or used the unsupported cloud-provider `configMapRef`.

## Diagnosis

```shell
kubectl --namespace <namespace> describe stageset <name>     # Message: which stage + operation
kubectl --namespace stageset-system logs deploy/stageset-controller --tail=200

# For apply/verify failures, inspect what the stage tried to apply:
kubectl --namespace <namespace> get stageinventory \
  --selector stages.metio.wtf/stage-set=<name>,stages.metio.wtf/stage=<stage>
```

## Remediation

Match the operation in the Message:

- **fetch / digest** — confirm the producer republished cleanly; a digest mismatch means the artifact changed mid-flight or is corrupt.
- **build** — validate the manifests/patches locally; ensure every `substituteFrom` ConfigMap/Secret exists.
- **apply RBAC** — grant the impersonated `serviceAccountName` (or the controller) the verbs it was denied; the Message names the resource.
- **apply immutable conflict** — set a per-stage [`conflictPolicy`](/usage/conflict-policies/) (or `force: true`, its blunt `Recreate`-everything form) so the controller deletes and recreates the conflicting object; for objects holding data (`PersistentVolumeClaim`/`PersistentVolume`) a `Recreate` rule additionally requires `allowDataLoss: true`. Alternatively, use content-hash-suffixed names so a change is a new object rather than a mutation.
- **verify timeout** — raise the stage `timeout`, or fix why the workload is not becoming Ready.
- **action** — read the action's error; for `http`, confirm the host is in `--allowed-action-hosts`.

Retries re-run the same pinned snapshot idempotently — actions already recorded in the stage's ledger do not re-fire. See [stages and sources](/usage/stages-and-sources/) for how a stage resolves and applies.
