---
title: Rollback
description: Restore the last good artifact revision when a run fails.
tags: [rollback, stages, versioning]
---

When a run fails, the controller can restore the last successfully-applied artifact
revisions instead of leaving you on a broken release. Rollback is opt-in and needs
somewhere to keep prior revisions.

## Enabling it

```yaml
spec:
  rollbackOnFailure: true
  stages:
    - name: app
      sourceRef:
        name: my-app
```

On a failed run the controller restores each stage's last-good artifact revision,
best-effort, and emits a `RolledBack` event. The coordinates it restores from are
recorded in `status.lastAppliedSnapshot`.

## The rollback store

Rollback needs the prior revision to still be fetchable, so the controller keeps a
copy in a **rollback store**. Configure one on the controller (cluster-wide), via
either a shared filesystem or S3:

```text
# filesystem (an RWX PersistentVolumeClaim)
--rollback-store-path=/var/lib/stageset/rollback

# or S3-compatible object storage
--rollback-store-s3-endpoint=s3.example.com
--rollback-store-s3-bucket=stageset-rollback
```

The two are mutually exclusive. With no store configured, rollback can only use a
prior revision the producer itself still retains; a dedicated store makes rollback
reliable across producer pruning.

### Encryption at rest

The store keeps each stage's rendered output, which includes any `Secret`'s data —
including [SOPS](https://github.com/getsops/sops)-decrypted values (see
[secrets encryption](/usage/encryption/)). Treat it as sensitive and keep it
encrypted at rest:

- **S3** encrypts by default. `--rollback-store-s3-sse` (chart:
  `rollbackStore.s3.sse`) is `s3` (SSE-S3) out of the box; set `kms` with
  `rollbackStore.s3.sseKmsKeyId` for SSE-KMS, or `none` only for a backend that
  cannot honor an SSE header. A rejected SSE write is non-fatal — it warns via a
  `RollbackStoreFailed` event and skips the store write; the rollout still
  succeeds.
- **Filesystem** can't encrypt itself — back the PVC with an **encrypted volume**
  (an encrypted `StorageClass`, LUKS, or cloud-disk encryption). The controller
  logs a reminder at startup when the file store is enabled.

If a restore can't proceed because the previous revision is gone, the run fails
with the `PreviousRevisionUnavailable` reason (see its
[runbook](/runbooks/previousrevisionunavailable/)), and a store problem surfaces as
a `RollbackStoreFailed` event.
