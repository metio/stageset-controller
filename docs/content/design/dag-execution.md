---
title: "DAG execution between stages"
weight: 2
---

# Design: DAG Execution Between Stages

| | |
|---|---|
| **Status** | Proposal (Future Work) |
| **Kind** | `StageSet` (`stages.metio.wtf/v1`) |
| **Audience** | maintainers |
| **Depends on** | the v1 reconciliation model in [stageset.md](stageset.md) |

## Motivation

v1 runs stages strictly sequentially: stage *i+1* starts only once stage *i* is
Ready. Independent work serializes needlessly — a CRD bundle, an operators
bundle, and a dashboards bundle that share no dependency still run one after
another, and total rollout latency grows linearly in stage count. A DAG lets
independent stages run **in parallel** and serialize **only where there is a
real dependency**, converging at join points.

This is a *fine-grained, intra-StageSet* DAG. It is complementary to — not a
replacement for — the *coarse-grained, cross-object* DAG that Flux already
offers via `Kustomization.dependsOn` and that StageSet offers via
`spec.dependsOn` between StageSets.

### Non-goals

- Cross-StageSet ordering — that is `spec.dependsOn`, which exists.
- Changing the default. DAG is **opt-in**; existing StageSets keep exact
  sequential semantics with zero spec change.
- Distributed execution. A StageSet is still reconciled by one controller
  replica (leader); the DAG is scheduled inside one reconcile.

## API

A top-level mode plus a per-stage dependency list:

```yaml
spec:
  executionMode: Graph        # Sequential (default) | Graph
  maxConcurrentStages: 4       # optional; 0 = unbounded within a ready set
  stages:
    - name: crds               # root (no needs): runs first
    - name: operators
      needs: [crds]
    - name: dashboards
      needs: [crds]            # operators and dashboards run in parallel
    - name: smoke
      needs: [operators, dashboards]   # join: waits for both
```

Semantics:

- **`executionMode: Sequential` (default)** is exactly v1. `needs` is ignored
  (rejected by validation if present, to avoid silent no-ops). Existing
  StageSets are byte-for-byte unaffected.
- **`executionMode: Graph`**: a stage runs once every stage in its `needs` is
  Ready. A stage with no `needs` is a **root** and runs in the first wave. The
  `spec.stages` list order is declaration order only; execution order is the
  topological order induced by `needs`.

Choosing an explicit `executionMode` over *inferring* a DAG from the presence of
any `needs:` is deliberate — see [critical review §9](#9-backward-compatibility).

### Validation (admission webhook + `ValidateSpec` fallback)

Terminal (`reason=InvalidSpec` / `Stalled`), never requeued:

1. In `Sequential` mode, no stage may set `needs`.
2. Every `needs` entry names an existing stage; no self-reference.
3. The graph is **acyclic** (Kahn's algorithm; a residual non-empty set is a
   cycle → `Stalled`).
4. **No two stages that are not connected by a `needs` path may apply the same
   object** (GVK + namespace + name). This is the load-bearing safety check —
   see [critical review §1](#1-the-double-owner-race). Object sets are known
   only after BUILD, so this is enforced in a build-all-before-apply phase (see
   Execution model).

## Execution model

1. **Resolve + pin** every stage's revision at run start — *unchanged*. The DAG
   reorders execution; it does not change the pinned snapshot. The "one
   consistent revision set" atomicity property of v1 holds.
2. **Build the DAG** from `needs`; validate acyclic.
3. **Build-all, then apply.** In `Graph` mode the pipeline splits: every stage
   is fetched + built first (producing its object set), the cross-stage
   double-owner check (validation §4) runs against all object sets, and only
   then does scheduling/apply begin. (In `Sequential` mode BUILD stays
   interleaved with APPLY as in v1 — no behavior change.)
4. **Schedule.** Maintain a ready-set (stages whose `needs` are all Ready). Run
   ready stages concurrently through a bounded worker pool
   (`errgroup` + semaphore sized by `maxConcurrentStages`). As each stage
   reaches Ready, re-evaluate dependents and admit newly-ready stages. Each
   stage's internal pipeline — PRE → APPLY → (deferred PRUNE) → VERIFY → POST —
   is unchanged.
5. **Prune once, at the end.** The single cross-stage prune pass after *all*
   stages complete is unchanged and is in fact *required*: ownership transfer
   (an object moving from stage A to stage B) must diff against the run's final
   inventory, exactly as today.
6. **Assemble status after the wait.** Stage goroutines return results; the
   reconcile (single owner of status) assembles `status.stages`, conditions,
   and events after the worker pool drains. Stage goroutines never mutate shared
   status — see [critical review §8](#8-status-and-event-determinism).

The reconcile drives the whole DAG to completion or failure within one
`Reconcile()` call, exactly as the sequential model already does (it blocks on
each stage's VERIFY wait today). DAG parallelizes *within* that block; it does
not change the synchronous-run model.

## Failure semantics

**Fail-fast with branch quiescence.** On a stage failure: schedule no new
stages; let in-flight stages finish (cancelling a stage mid-APPLY is unsafe —
SSA is not transactional); then halt with `Ready=False reason=StageFailed`
naming the failed stage. Stages on already-completed branches stay applied —
the same shape of partial state the sequential model already produces on
failure, just potentially wider. `rollbackOnFailure` mitigates (below).

Rejected for the first release: *continue independent branches past a failure*.
It leaves a harder-to-reason partial state and badly complicates rollback
ordering; deferred to a possible Phase 2.

## Interaction with existing features

- **Revision pinning / atomicity** — unchanged. Pin all at start; the DAG only
  reorders.
- **Inventory & pruning** — the deferred single-pass prune is unchanged and
  required (ownership transfer is whole-run). The new hazard is two *parallel*
  stages applying the same object; validation §4 makes that impossible by
  construction (same-object-across-stages is permitted only along a `needs`
  edge, i.e. sequentially — the ownership-transfer case).
- **Stage actions (pre/post/onFailure)** — per-stage, unchanged. The action
  ledger is already keyed by `(stage, revision)`, so concurrent stages' ledgers
  don't collide. Actions are already required to be idempotent.
- **Versioned migrations** — anchored to a stage, run before it. Migrations on
  *parallel* stages run concurrently; the controller cannot prove two
  migrations are data-independent, so **interacting migrations must be ordered
  with `needs`**. This is a documented operator responsibility; validation can
  only warn (not prove) when two parallel stages both declare migrations.
- **`rollbackOnFailure`** — the snapshot is already per-stage
  (`status.lastAppliedSnapshot`, with the #3 substitution fingerprint). Rollback
  re-applies "in forward order"; in `Graph` mode *forward order* is redefined as
  **topological order**, and the prune-safe restore order is **reverse
  topological**. Phase 1 keeps rollback semantics otherwise identical (restore
  the recorded per-stage snapshot, just ordered by the graph); richer
  partial-DAG rollback is Phase 2 — see [critical review §7](#7-rollback-under-a-partial-dag).
- **Update windows / `dependsOn` / drift (`driftDetectionInterval`) / dynamic
  producer watches** — run-level or per-source, orthogonal to execution order;
  unchanged.
- **Flagger stage-gate webhook** — `200 = stage phase is Ready at the pinned
  revision`. A stage's Ready is a well-defined DAG node regardless of how the
  graph reached it, so the "migration stage green before Canary promotes"
  pattern is unaffected.

## Flux interaction

- A StageSet is a single CR with **one** `Ready` condition; the DAG is
  *internal*. Everything downstream in Flux — `notification-controller` alerts,
  another object's `dependsOn` on this StageSet — sees only the aggregate Ready.
  The intra-DAG structure is invisible to Flux, so it composes cleanly with
  Flux's coarse `dependsOn`.
- **Two complementary layers.** Flux's `Kustomization.dependsOn` is a DAG
  *between* reconciled objects; StageSet `needs` is a DAG *within* one object.
  Neither subsumes the other; both can be used together (a StageSet that itself
  `dependsOn` another, and internally fans out).
- **ExternalArtifact / source-controller** — pinned at run start; the DAG does
  not touch the fetch / digest-verify / build path or source-controller's
  contract.
- **Flagger Canary** — gating queries a stage node's readiness (above). A Canary
  promotion that wakes a mid-flight DAG run relies on idempotent resume
  ([§3](#3-resume-correctness)).

## JaaS interaction

- JaaS `JsonnetSnippet`s publish `ExternalArtifact`s that stages consume. A DAG
  where several stages consume *different* JaaS snippets in parallel is fine:
  each stage resolves its `sourceRef → ExternalArtifact` independently
  (producer-aware resolution is read-only and per-stage).
- The dynamic producer watch (see stageset.md) surfaces a snippet's failure on
  the StageSet immediately, regardless of which DAG node consumes it — the
  watch is per producer GVK, not per stage.
- **Rollback + JaaS retention.** Rollback re-fetches pinned revisions; in
  `Graph` mode the re-fetch order is topological. The requirement that JaaS
  snippets used with `rollbackOnFailure` keep `spec.history >= 2` is unchanged.
- **Producer-aware cross-reference** (a stage names a `JsonnetSnippet`, resolved
  via the RFC-0012 back-pointer) is per-stage and unaffected by the DAG.

## Critical self-review

### 1. The double-owner race

*The single biggest hazard.* Two parallel stages applying the **same** object
would each record it in their `StageInventory`, making the prune diff ambiguous
about which stage owns it. (At the SSA layer there is no conflict — the whole
controller applies under one field manager, `stageset-controller` — which is
*why* the hazard is silent: nothing errors, the inventory just becomes
incoherent.) **Resolution:** validation §4 forbids two non-`needs`-connected
stages from applying the same object; same-object-across-stages is allowed only
along a dependency edge (the sequential ownership-transfer case). This makes the
race impossible by construction.

The cost: the check needs every stage's built object set *before* any apply, so
`Graph` mode must **build-all-then-apply** rather than interleave build and
apply per stage. That raises peak memory (all rendered object sets held at once)
and means a build failure in any stage aborts before *any* apply — arguably a
feature (no partial apply from a run that couldn't build). `Sequential` mode is
untouched. The alternative — lazy detection (the second concurrent claimant
fails) — is simpler but *nondeterministic* (the failure depends on goroutine
timing), which is unacceptable for a deployment tool. Build-all-then-apply wins.

### 2. Atomicity under partial failure is weaker than it looks

The sequential model already leaves earlier stages applied on failure; the DAG
makes "earlier" mean "every completed branch," a wider and more heterogeneous
partial state. This is the same *kind* of partial state, not a new one, and
`rollbackOnFailure` addresses it — but it must be stated plainly: **a DAG widens
the partial-failure surface.** Operators who need stricter all-or-nothing should
keep the run sequential or enable `rollbackOnFailure`. The design does not, and
cannot cheaply, offer transactional multi-object apply (Kubernetes has no such
primitive).

### 3. Resume correctness

A reconcile drives the DAG synchronously; a process restart mid-run must resume
cleanly. On resume the controller reconstructs the ready-set from
`status.stages[].phase` + applied revisions (already persisted) and continues. A
stage that was mid-APPLY when the process died re-runs from PRE; SSA is
idempotent and actions are idempotent (ledger keyed by `(stage, revision)`), so
re-execution is safe. Resume is *more* complex than the linear case (the
ready-set must be rebuilt), but it needs no new persisted state. Verified-safe
in principle; the test burden is real (kill-and-resume at each DAG position).

### 4. Synchronous whole-DAG vs level-per-reconcile

Driving the whole DAG in one `Reconcile()` blocks the worker for the run's
duration — but the **sequential model already does this** (it blocks on each
VERIFY wait). DAG only parallelizes within that block. The multiplier is
in-flight work: N concurrent VERIFY waits instead of one. `maxConcurrentStages`
bounds it within a StageSet; `MaxConcurrentReconciles` bounds StageSets. The
alternative — apply one topological *level* per reconcile and requeue —
unblocks the worker between levels but adds requeue latency per level (slower
rollouts) and complicates status/resume. Lean synchronous to match v1; revisit
only if very long DAGs starve the work queue.

### 5. Cycles and dangling edges

A `needs` cycle is unrunnable → rejected at admission (`Stalled`). A `needs`
entry naming a removed stage is dangling → rejected. Both are terminal (waiting
for a spec change), consistent with the existing `dependsOn`-cycle handling.

### 6. Migration ordering

Two parallel stages whose migrations touch the same data race, and the
controller **cannot prove** independence. Mitigation is a documented contract:
order interacting migrations with `needs`. Validation can warn when two parallel
stages both declare migrations but cannot reject (it would forbid legitimately
independent parallel migrations). This is an honest gap, not a solved problem —
the controller trades safety here for the operator's domain knowledge, the same
trade the existing `substituteFrom`-changed rollback contract makes.

### 7. Rollback under a partial DAG

The hardest corner. If branch B fails after branches A and C applied, a faithful
rollback must restore A, C (and any of B's applied stages) to the previous
snapshot in **reverse-topological** order so a prune never deletes an object a
not-yet-rolled-back stage still owns. Stages with *no* previous snapshot entry
(first-ever run) have nothing to restore and are pruned. The bookkeeping is
real. **Phase 1 deliberately keeps it minimal:** rollback restores the recorded
per-stage snapshot, ordered topologically — no new partial-DAG semantics beyond
ordering — and documents that a partial-DAG rollback with heterogeneous
first-run/Nth-run stages is best-effort. A complete partial-DAG rollback is
Phase 2. If even the ordered-restore proves subtle in review, the safest first
cut is to **disallow `rollbackOnFailure` together with `executionMode: Graph`**
until Phase 2 — ship parallelism and rollback independently rather than ship a
rollback that silently misorders.

### 8. Status and event determinism

Parallel stages complete in nondeterministic order, so events and status updates
would interleave and race the shared status struct. **Resolution:** stage
goroutines are pure — they return `(phase, inventory, error)` and never touch
`ss.Status`; the reconcile assembles status and emits events *after* the worker
pool drains, in a stable (declaration or topological) order. No concurrent
mutation, deterministic event order. This is a hard rule, not a nicety — a
shared-status data race would be a `-race` failure and a correctness bug.

### 9. Backward compatibility

If `needs` were *inferred* (a DAG whenever any stage sets `needs`, sequential
otherwise), an operator adding one `needs` to an existing StageSet would
silently flip every other stage from sequential to parallel — a surprising,
possibly unsafe behavior change. The explicit `spec.executionMode` (default
`Sequential`) avoids this: opting into the graph is a visible, reviewable spec
edit, and `Sequential` is preserved bit-for-bit. The cost is one extra field;
the safety is worth it.

### 10. Flagger / Canary timing

A Canary gating on a stage reachable via multiple branches still queries that
stage's well-defined Ready node — no ambiguity. A promotion that wakes a
mid-flight DAG run depends only on idempotent resume (§3). No new hazard.

## Recommendation and phasing

**Phase 1.** `spec.executionMode: Graph` + `Stage.needs`; validation (acyclic,
refs exist, no `needs` in `Sequential`, the double-owner check); build-all-then-
apply; leveled concurrent scheduling bounded by `maxConcurrentStages`;
fail-fast-with-quiescence; status assembled post-wait. For rollback, **either**
order the recorded-snapshot restore topologically **or** (the conservative cut)
reject `rollbackOnFailure` + `Graph` until Phase 2 — decide in implementation
review, leaning conservative. Migrations: documented `needs`-ordering contract.

**Phase 2 (demand-driven).** Continue-independent-branches-past-failure; full
partial-DAG rollback; optional per-level requeue for very long runs.

The default stays `Sequential`: **zero behavior change for every existing
StageSet.** DAG is a capability you opt into, reviewed one field at a time.
