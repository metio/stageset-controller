# Reason: PreviousRevisionUnavailable

## Symptom

`READY=False`, `REASON=PreviousRevisionUnavailable`. The StageSet has `spec.rollbackOnFailure` set, a run failed, and the controller could not restore the last-good revisions.

## Cause

`rollbackOnFailure` restores the previously-applied artifact revisions by re-fetching their recorded URLs and verifying their digests. That only works while the **producer still retains** those revisions. This reason means a revision the rollback needs is no longer fetchable — the producer garbage-collected it.

Rollback is best-effort by contract: it works exactly when producers retain. Common triggers:

- a JaaS `JsonnetSnippet` with `spec.history: 1` (the default) — only the current revision is kept, so there is no previous revision to roll back to
- a stock source-controller source, which retains only the current revision
- the previous revision aged out of the producer's retention window

## Diagnosis

```shell
kubectl describe stageset <name> -n <namespace>   # Message names the stage + revision
```

Check the producer's retention. For a JaaS snippet:

```shell
kubectl get jsonnetsnippet <name> -n <namespace> -o jsonpath='{.spec.history}'
```

## Remediation

The cluster is left at the partially-applied failed state; resolve the underlying failure (see the failing stage's own runbook) and fix forward — the StageSet converges once the desired revision applies cleanly.

To make rollback reliable in future, either:

- **Increase producer retention** so at least one previous revision is always fetchable — JaaS snippets used with `rollbackOnFailure` should set `spec.history: 2` (or more); sources that retain only the current revision cannot support the re-fetch path, so rely on source revert instead.
- **Configure the external rollback store** — a filesystem/RWX PVC (`--rollback-store-path`) or an S3 bucket (`--rollback-store-s3-*`). When the controller pushes rendered output to a store it owns, rollback is bit-exact and **independent of producer retention** — this `PreviousRevisionUnavailable` state cannot occur for runs the store holds.
