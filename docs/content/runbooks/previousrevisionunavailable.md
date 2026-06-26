---
title: PreviousRevisionUnavailable
description: rollbackOnFailure is set but the last-good revisions could not be restored.
tags: [runbooks, rollback, troubleshooting]
---

## Symptom

`READY=False`, `REASON=PreviousRevisionUnavailable`. The StageSet has `spec.rollbackOnFailure` set, a run failed, and the controller could not restore the last-good revisions.

## Cause

[`rollbackOnFailure`](/gating/rollback/) restores the previously-applied artifact revisions by re-fetching their recorded URLs and verifying their digests. That only works while the **producer still retains** those revisions. This reason means a revision the rollback needs is no longer fetchable — the producer garbage-collected it.

Rollback is best-effort by contract: it works exactly when producers retain. Common triggers:

- a JaaS `JsonnetSnippet` with `spec.history: 1` (the default) — only the current revision is kept, so there is no previous revision to roll back to
- a stock source-controller source, which retains only the current revision
- the previous revision aged out of the producer's retention window
- the run used [SOPS decryption](/security/encryption/) and the key Secret was rotated
  or deleted — rollback re-runs decryption rather than restoring plaintext, so it
  fails closed when the key is gone, even for a revision the rollback store holds

A configured external rollback store changes the failure shape rather than masking it:

- a **transient store outage** (an S3 5xx, a connection reset) is _not_ reported as `PreviousRevisionUnavailable`. The controller emits a `RollbackStoreFailed` Warning Event and backs off so the rollback retries once the store recovers — it never silently re-fetches the producer, which could have already garbage-collected the revision and turned a recoverable blip into a false terminal.
- a **corrupt snapshot** in the store (the object decodes to garbage) emits a `RollbackStoreFailed` Warning Event and falls back to a producer re-fetch. If the producer still retains the revision, rollback succeeds; if not, this terminal reason is set.

## Diagnosis

```shell
kubectl --namespace <namespace> describe stageset <name>   # Message names the stage + revision
kubectl --namespace <namespace> get events --field-selector reason=RollbackStoreFailed
```

Check the producer's retention. For a JaaS snippet:

```shell
kubectl --namespace <namespace> get jsonnetsnippet <name> --output jsonpath='{.spec.history}'
```

## Remediation

The cluster is left at the partially-applied failed state; resolve the underlying failure (see the failing stage's own runbook) and fix forward — the StageSet converges once the desired revision applies cleanly.

To make rollback reliable in future, either:

- **Increase producer retention** so at least one previous revision is always fetchable — JaaS snippets used with `rollbackOnFailure` should set `spec.history: 2` (or more); sources that retain only the current revision cannot support the re-fetch path, so rely on source revert instead.
- **Configure the external rollback store** — a filesystem/RWX PVC (`--rollback-store-path`) or an S3 bucket (`--rollback-store-s3-*`); see [operations](/running/operations/). When the controller pushes rendered output to a store it owns, rollback is bit-exact and **independent of producer retention** — this `PreviousRevisionUnavailable` state cannot occur for runs the store holds, unless the run used SOPS and the key Secret is no longer readable (see the SOPS trigger above).
