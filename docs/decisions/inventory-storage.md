# Inventory storage and modes

## Decision

Each stage's applied object set is recorded in a dedicated namespaced CRD, `StageInventory` (`stages.metio.wtf/v1`), holding only object identifiers — never manifests — sharded across multiple objects (`--inventory-shard-cap`, default 5000 entries/shard) and owned by the StageSet for GC safety. Pruning is authoritative against these explicit entries.

`--inventory-mode` selects how membership is also expressed on the objects themselves:

- `entries` — identifiers in `StageInventory` only.
- `hybrid` (default) — entries plus an ApplySet (KEP-3659) `applyset.kubernetes.io/part-of` member label, so `kubectl get -l …` answers "what does this stage own" with no project-specific tooling.
- `applyset` — reserved for a future entry-free, discovery-based mode.

## Context and alternatives

**Why a CRD, not in-status inventory.** Helm-style storage (an entire gzipped manifest set in one object) is a known scaling pain. Inventory must survive controller restarts, support concurrent-safe updates, be inspectable with standard tooling, and scale past the single-object size ceiling. A sharded identifier-only CRD meets all four; keeping manifests out of it keeps each shard small.

**Why entry-based pruning, not label discovery.** A purely label-based ApplySet prune (LIST every group-kind, delete anything bearing the label that is no longer desired) fails open on label stripping — an object whose label was removed out-of-band is silently orphaned with no record it ever existed — and its server-side cost scales with *cluster* size rather than *stage* size. Authoritative entry diffing has neither problem. The ApplySet labels are still emitted in `hybrid` mode for observability and as the foundation for a future opt-in `applyset` mode; they are not the source of truth for pruning.

**Cross-stage ownership transfer.** Pruning runs as a single end-of-run pass over all stages' inventories (via the tested `inventory.ComputePlan`), so an object that moves from one stage to another in a single change transfers ownership instead of being deleted and recreated. A single object claimed by two stages is an ambiguous spec and stalls the run.

See [`../content/design/stageset.md`](../content/design/stageset.md) (Inventory Storage, Pruning semantics) for the shard layout and prune algorithm.
