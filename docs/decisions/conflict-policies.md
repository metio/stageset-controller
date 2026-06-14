# Immutable-field conflict handling

## Decision

`stage.conflictPolicy` gives a per-resource answer to an immutable-field conflict: `Fail` (default), `Recreate` (delete + re-apply), or `KeepExisting` (create if absent, never update). Each object's action is resolved by precedence — a `stages.metio.wtf/force: enabled` annotation wins, then the first matching rule by target, then the effective default (`conflictPolicy.default`, or `Recreate` when `stage.force` is set, else `Fail`). The blunt `stage.force` toggle becomes sugar for `default: Recreate`.

The resolved action is realized through fluxcd `ssa`'s selectors: `Recreate` objects carry a marker matched by `ApplyOptions.ForceSelector`; `KeepExisting` objects carry one matched by `IfNotPresentSelector`. The apply engine does the per-object work — no bespoke conflict loop.

A `Recreate` rule that targets a PersistentVolumeClaim or PersistentVolume is **refused unless the rule sets `allowDataLoss: true`**.

## Context and alternatives

Most immutable-field failures (immutable Secrets/ConfigMaps, Job pod templates, Service `clusterIP`, workload selectors, PVC storage classes) are mechanical: the operator needs a per-resource answer to "the apiserver said no", not a single blunt switch. The existing `stage.force` recreated *everything* on conflict, which is both too coarse and dangerous for stateful objects.

**Why selectors, not a hand-rolled conflict loop.** `ssa` already classifies and force-applies per object via label/annotation selectors — the same mechanism kustomize-controller uses for per-object force. Stamping markers and letting the engine apply them is less code and inherits the engine's conflict semantics (immutable-field SSA conflicts *and* `field is immutable` Invalid rejections) for free.

**Why `allowDataLoss` gates only rule-driven Recreate.** Recreating a PVC/PV deletes data, so it must be said out loud — but only when it comes from a *rule*. The blunt `stage.force` and the explicit per-object annotation are treated as the operator having already opted in, and are not gated; gating them would silently break the existing `force` behavior. The gate lands exactly where the design places it: "unless the rule sets `allowDataLoss: true`".

The recommended pattern is still to *avoid* conflicts: content-hash-suffixed names for immutable Secrets/ConfigMaps, where a change is a new object plus pruning of the old — handled natively by the inventory diff with no recreate gap.

See [`../content/design/stageset.md`](../content/design/stageset.md) (Conflict Policies) for the full rule schema.
