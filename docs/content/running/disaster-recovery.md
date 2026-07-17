---
title: Backup and disaster recovery
description: Back up the state the controller cannot fully reconstruct, and restore a cluster in the order that keeps pruning, teardown, and decryption safe.
tags: [backup, disaster-recovery, restore, inventory, encryption, operations]
---

Back up the small set of state the controller cannot reconstruct, then rebuild a
lost cluster by restoring that state in the right order. Most of what a `StageSet`
manages is recomputed on the next reconcile from your Git repository and your Flux
sources, so it needs no backup of its own. Three things are the exception, and
losing any of them causes silent data damage rather than a clean failure.

## What the controller rebuilds, and what it cannot

A `StageSet` is a function of its source content: given the same spec and the same
artifacts, a reconcile re-fetches, re-renders, and re-applies the same objects.
The applied workloads, the `StageSet` status, the webhook's self-signed CA, and the
leader lease are all reconstructed automatically. Four pieces of state need
backup attention — the first self-heals for the common case, the next two are
unrecoverable if lost, and the fourth applies only when a stage uses a once-ever
action:

1. **`StageInventory` resources** — the record of exactly which objects each stage
   owns, read to prune and to tear down. If it is lost while the stage's objects are
   still live, the controller **self-heals**: the next reconcile rebuilds the
   inventory from the objects' owner and per-stage labels (across the GVKs the
   current render touches), emits an `InventoryReconstructed` event, and **defers
   pruning that pass** so it never deletes against a best-effort rebuild; the
   following reconcile prunes normally. The residual gap is a kind the current
   render no longer contains at all — those objects still carry the labels but
   aren't swept, so they stay orphaned. Back up the inventory so that gap, and
   reverse-order teardown, stay correct. It lives in `spec` precisely so a
   spec-restoring backup tool captures it.
2. **Encryption key Secrets** — the age or PGP private keys named by
   `spec.decryption.secretRef`. They are unrecoverable if lost. A
   [rollback](/gating/rollback/) that must re-decrypt a prior revision fails closed
   with reason `PreviousRevisionUnavailable`, and any new run that decrypts SOPS
   files fails until the keys are restored. Cloud-KMS keys live outside the cluster;
   back up the KMS key policy or ARN instead of the key material.
3. **Rollback store contents** — when `spec.rollbackOnFailure: true` and the
   producer retains only one revision, the [rollback store](/gating/rollback/) (a
   RWX `PersistentVolume` or an S3 bucket) holds the sole copy of the last-good
   rendered output. It is encrypted at rest because it contains decrypted Secrets.
4. **`StageLedger` completions** — when a stage uses a
   [`scope: Lifetime`](/defining-a-release/actions/#run-once-ever-scope-lifetime)
   action, its once-ever completion lives in a `StageLedger`. Entries the operator
   asserted via `spec.baseline` come back from Git; entries the controller ran
   itself (`origin: Executed`) live only in etcd. A cluster rebuilt from Git alone
   would re-run that bootstrap against state that survived the disaster — an
   external database, an object store — so export the completions to a committable
   baseline as part of your backup routine (below).

## State inventory

| Component | Source of truth | Regeneratable? | Back up? |
|---|---|---|---|
| `StageSet` spec | your Git repository / Flux source | yes — re-applied by Flux | through the GitOps repo |
| `StageSet` status | rebuilt by reconcile | yes — but `status.lastAppliedSnapshot` is the rollback target | via an etcd snapshot |
| **`StageInventory`** | the controller (cluster state only) | **best-effort self-heal from live objects** | **recommended (covers the residual-GVK gap + teardown)** |
| **`StageLedger`** | `spec.baseline` in Git + controller-run `status` in etcd | **partly — Baselined entries from Git; Executed entries need an export or etcd snapshot** | **yes if any stage uses `scope: Lifetime`** |
| **Encryption key Secrets** | `spec.decryption.secretRef` | **no — unrecoverable** | **yes (critical)** |
| **Rollback store** | RWX PVC or S3 bucket | **no when it is the only copy** | **yes if `rollbackOnFailure` is used** |
| Applied workloads | stage sources | yes — re-applied from sources | no |
| Webhook self-signed CA | the controller | yes — regenerated on startup | no |
| Leader lease | the controller | yes — re-elected | no |

## What to back up

- **Your GitOps repository.** It holds every `StageSet` spec and the Flux source
  objects (`GitRepository`, `OCIRepository`, `Bucket`, `ExternalArtifact`
  producers) the stages reference. This is the primary source of truth — keep it
  backed up the way you back up any Git repository.
- **`StageInventory` resources.** These are cluster state, not in Git. Capture them
  with a cluster backup tool (for example Velero) or an etcd snapshot. Because the
  inventory deliberately lives in `spec` — not `status` — any backup tool that
  restores `spec` captures the prune history. The relevant scope is every
  `StageInventory` in each `StageSet`'s namespace.
- **Encryption key Secrets.** Back up the Secrets named by
  `spec.decryption.secretRef` in each `StageSet`'s namespace. For a cloud-KMS-only
  setup, back up the KMS key policy and ARN rather than key material.
- **Rollback-store contents and credentials**, if `spec.rollbackOnFailure: true`.
  Snapshot the RWX volume or replicate the S3 bucket, and back up the bucket
  credentials Secret. Skip this if every producer retains more than one revision —
  rollback then re-fetches from the producer and the store is only a fast path.
- **`StageLedger` completions**, if any stage uses `scope: Lifetime`. Commit the
  ledger's `spec.baseline` alongside its `StageSet`, and export the
  controller-run completions periodically so a Git rebuild does not re-run a
  bootstrap whose effect survived the disaster:

  ```shell
  stagesetctl baseline <name> --export > <name>-ledger.yaml
  ```

  On restore, apply the exported ledger before the `StageSet` reconciles (below).
  Its entries return as `origin: Baselined` — the guarantee survives, the
  provenance downgrades honestly to "we assert this ran".

## Rebuilding a cluster

Restore in the order below. The order matters: the encryption keys and the
inventory must exist **before** the controller reconciles a `StageSet`, or the first
reconcile does damage.

1. **Install the controller** from the Helm chart. See
   [Production](/running/production/) for the hardened install.
2. **Restore the encryption key Secrets.** Keys must exist before any reconcile,
   because a stage that decrypts SOPS files fails until its `secretRef` Secret is
   present, and a `rollbackOnFailure` restore that re-decrypts a prior revision
   fails with `PreviousRevisionUnavailable` without them. See
   [Secrets encryption](/security/encryption/).
3. **Restore the rollback-store credentials and contents** if you use one, so a run
   that fails immediately after rebuild can still restore its last-good output. See
   [Rollback](/gating/rollback/).
4. **Restore or re-sync the Flux sources.** Let Flux reconcile the
   `GitRepository` / `OCIRepository` / `Bucket` objects and the producers so each
   stage's `ExternalArtifact` becomes `Ready` again.
5. **Restore the `StageSet` resources** — normally by letting Flux apply them from
   your GitOps repository.
6. **Restore the `StageInventory` resources** before the controller reconciles the
   `StageSet`s, so the first reconcile can prune correctly and a later deletion can
   tear down. Restoring the captured `spec` is sufficient — that is where the prune
   history lives. If a `StageSet` reconciles before its inventory is back, the run
   applies and verifies but cannot prune; objects a newer revision dropped are left
   orphaned until you restore the inventory and reconcile again.
7. **Restore the `StageLedger` resources** if any stage uses `scope: Lifetime` —
   apply the exported ledger (or `stagesetctl baseline <name> --stage S --action A`
   per completion) before the `StageSet` reconciles. Otherwise the first reconcile
   sees no completion and re-runs the once-ever bootstrap against state that
   survived the disaster.

If you suspend each `StageSet` (`spec.suspend: true`) before step 5 and resume only
after step 7, you remove the race entirely.

## Failure modes

| Lost item | What happens |
|---|---|
| `StageInventory` | the next reconcile applies and verifies, but the prune step has nothing to diff against, so removed objects are **orphaned**; `StageSet` deletion cannot reverse-order tear stages down |
| Encryption key Secret | new runs that decrypt SOPS files fail; a `rollbackOnFailure` restore that must re-decrypt fails with reason `PreviousRevisionUnavailable` |
| Rollback store | a rollback falls back to the producer's retained revisions; with single-revision retention there is nothing to restore and the rollback fails |

## Verifying recovery

After a rebuild, confirm the controller is back in a healthy steady state:

```shell
# Every StageSet reports Ready=True with reason Succeeded.
kubectl --namespace payments get stagesets

# The inventory is present for each stage.
kubectl --namespace payments get stageinventories
```

Then confirm pruning works end to end: change a stage's source so it renders one
fewer object, let the `StageSet` reconcile, and check the dropped object is gone. A
correct prune proves the restored inventory is intact — the controller's own
[disaster-recovery test](/contributing/building/) pins exactly this guarantee: with
the inventory restored, a removed object is pruned; with the inventory lost, the
same removed object is orphaned.

## Related

- [Inventory and pruning](/defining-a-release/inventory/) — what the `StageInventory` records and how the prune diff works.
- [Secrets encryption](/security/encryption/) — how `spec.decryption.secretRef` keys are read and used.
- [Rollback](/gating/rollback/) — the rollback store and `rollbackOnFailure` behavior.
- [Production](/running/production/) — hardening a production install.
- [Scale and capacity](/running/scale-and-capacity/) — sizing replicas and the state a multi-replica install keeps reachable.
- [`PreviousRevisionUnavailable` runbook](/runbooks/previousrevisionunavailable/) — remediating a rollback that cannot restore.
