<!--
SPDX-FileCopyrightText: The stageset-controller Authors
SPDX-License-Identifier: 0BSD
-->

# Sourced migrations — design doc

Status: **draft / not yet implemented / uncommitted.** Enriched after an
adversarial review (UX, security, ecosystem prior-art). Nothing here is built.

## Problem

Migrations today are an inline `[]Migration` on `StageSet.spec.migrations`
(`api/v1/stageset_types.go`). They are version-gated action ladders: each entry
has `name`, `to` (a concrete semver boundary), `from` (an optional semver
*constraint* on the currently-deployed version), `stage` (anchor — runs before
that stage's pre-actions), and `actions`. The controller
(`internal/controller/migrations.go`) resolves the desired version, selects
migrations where `current < to <= desired` (filtered by `from`), stable-sorts by
ascending `to`, runs them under the tenant ServiceAccount, and records progress
in a **per-StageSet** ledger (`status.executedMigrations`, keyed by migration
name, cleared once `status.version` reaches the target).

The dominant deployment shape is **one application deployed N times across N
namespaces, one StageSet per namespace** (multi-tenant: a platform team *and*
external companies author StageSets). With inline migrations the entire
destructive, order-sensitive ladder is copy-pasted into every StageSet — editing
4 of 5 copies and missing the 5th is a latent data-loss incident, and adding a
namespace duplicates the ladder again.

## Decision

Let a StageSet **source its migration ladder from a Flux source**, the same way
stages already source content — not via a new CRD or a cluster-scoped object.

```go
// On StageSet spec — mutually exclusive:
Migrations          []Migration                     `json:"migrations,omitempty"`           // inline ladder (the simple/self-contained case)
MigrationsSourceRef *MigrationsSource               `json:"migrationsSourceRef,omitempty"`  // Flux source + optional path + verification
```

The artifact contains a serialized `[]Migration` (YAML, or JSON as a jaas
`JsonnetSnippet` renders). The controller fetches it during version planning,
parses it into the existing `[]Migration`, and feeds it through the **unchanged**
`selectMigrations` / `runStageMigrations` / ledger machinery. The ladder lives
**once** in a registry/repo; each namespace holds only a small Flux source
pointer (an `OCIRepository`, exactly like every stage source) plus an identical
StageSet template. New migrations = push new content; the StageSet never changes.

### Rejected alternatives (and why)

- **Cluster-scoped `ClusterStageMigration`** selected by label or `application`.
  A destructive ladder that runs under a *namespaced* tenant SA must not be
  defined in a *cluster* object readable/writable across namespaces
  (confused-deputy / cross-tenant injection). This matches Tekton deprecating
  `ClusterTask` for the same security/necessity reasons. Cross-namespace sharing
  is provided better by the registry artifact itself.
- **Namespaced `StageMigration` wrapper CR** — unnecessary once the ladder can
  come from a source; the shareable thing is the artifact, not a k8s object.
- **Label selectors / `app.kubernetes.io/version` auto-detection** — the loosest,
  most mutable coupling for the resource that least tolerates ambiguity: a typo'd
  selector fails *open* (migration silently skipped), a `kubectl label` retargets
  a `DROP`, two apps named `api` cross-match. Selection of *which* destructive
  ladder runs must be explicit; the version math already lives in `selectMigrations`.
- **Per-stage migration lists** (`stage.migrations`) instead of one StageSet-level
  ladder. Migrations are a *version-axis* concern — selected by
  `current < to <= desired` and ordered by `to` across the whole transition —
  orthogonal to stage *topology* (BUILD→APPLY→PRUNE→VERIFY, `dependsOn` order).
  Putting them under each stage fragments the version history (no single place to
  read the app's ladder), hides global `to`-ordering behind stage structure, and —
  fatally — re-couples migrations to a specific stage layout, which breaks the
  shared-artifact model (one `[]Migration` consumed by N StageSets whose stages may
  differ). The legitimate pull behind per-stage — locality, and avoiding a brittle
  `stage:` string — is met instead by the late-binding anchor (below), which keeps
  the ladder in one shareable place while letting a stage declare where migrations
  attach.

**One thing this design gets right that no ecosystem tool has:** there is no
Kubernetes object holding *shared execution state* — N namespaces ⇒ N independent
executions (see guardrail 1). Structurally safer than any wrapper-CR/cluster
design.

## Prior art & adopted practices

Every mature system converges on the same rules; we adopt them.

| Source | What it does | What we take |
|---|---|---|
| **Helm hooks** | pre-upgrade `Job`, integer `hook-weight`, `hook-delete-policy`; rollback does **not** undo migrations; hooks re-run every upgrade | forward-only (downgrades already refused); idempotency is mandatory; **leave failed state visible** |
| **Argo CD** | `PreSync` phase (docs literally "DB schema migration before deploy") + integer `sync-wave`, each wave Healthy before next; failed PreSync aborts so the *old* app keeps serving the un-migrated schema | our stage-anchored, fail-closed-before-apply model is the same shape; **two status axes** (requested vs applied/healthy) |
| **Flux** | no native migration primitive (`flux#3264` unimplemented); people chain `Kustomization.dependsOn` + `wait`; **`OCIRepository.spec.verify`** (cosign/notation), `GitRepository.spec.verify` (GPG); **`Bucket` has no verify** | reuse `.spec.verify` for provenance; restrict migration sources to OCI/Git (or enforce digest for buckets) |
| **Atlas Operator** | `AtlasMigration` versioned ladder; directory in ConfigMap **or a tag/version-addressable registry**; `dev DB` for lint/diff; **dirty-state refusal**; `--baseline`; **approval gate** (`reason: ApprovalPending` + `approvalUrl`); two-layer `atlas.sum` (per-file hash + aggregate) | our OCI artifact is the open-infra equivalent of Atlas's registry (digest/tag-addressable, immutable, pull-at-deploy); adopt **dirty-state terminal condition**, **baseline ack**, **directory integrity sum**, and an **approval gate** (later phase) |
| **SchemaHero** | declarative; **plan → `kubectl schemahero approve migration` → apply** | optional approval gate for destructive transitions |
| **Flyway / Liquibase / golang-migrate / Atlas** | `(identity, checksum)` ledger that **fails closed on drift**; **never-edit-applied** enforced via that checksum; **baseline**; sticky **dirty/failed** state that halts auto-progress; **single-writer lock** (`DATABASECHANGELOGLOCK` / `pg_advisory_lock`). golang-migrate skipping checksums is the documented foot-gun | **`(name, content-digest)` ledger in v1**; append-only enforced by it; baseline; dirty terminal state; single-writer (controller leader-election + optional DB advisory lock) |

## How it works

Unchanged: the version axis (`spec.version` `VersionSource` → `status.version`),
`current < to <= desired` selection, downgrade refusal
(`DowngradeRequiresMigration`), baselining, stage-anchored execution, the
per-StageSet ledger.

Changed: `planVersionMigrations` builds its `[]Migration` from **either**
`spec.migrations` **or** the artifact fetched from `spec.migrationsSourceRef`,
then runs the existing selection. "Does this transition need a migration?" is
answered by the resolved ladder's `to` fields plus the transition — there is **no
separate registry of which versions need migrations**. Safety comes from failing
closed on an *unresolved or unverified* source, so "ladder present, none in
range" (legitimately nothing) is distinguishable from "ladder not loaded/trusted
yet" (wait).

## Correctness guardrails (all v1 — these are the cost of entry, not fast-follows)

1. **Definition shared, execution not.** The artifact is pure data; each StageSet
   runs against its own DB and records in its own `status.executedMigrations`.
   **N namespaces ⇒ N independent executions.** No k8s object holds shared
   execution state. *Test:* two StageSets sourcing the same artifact against two
   fake DBs run the action twice.
2. **`(name, content-digest)` ledger keying.** Key the executed-ledger on the
   migration name *and* a digest of its resolved actions (the Flyway/Liquibase/
   Atlas checksum rule). An edited sourced migration becomes a new, unexecuted
   migration — never a silent skip or silent re-run. *(Was an open question; the
   review and every prior-art tool make it mandatory; golang-migrate's omission
   is the cautionary tale.)*
3. **Intra-migration per-action idempotency.** Today `runStageMigrations` passes
   the executor an *empty* ledger and a no-op recorder, so a retry of a
   half-failed ladder **re-runs already-completed destructive actions**. Give
   migration actions the same per-action ledger stages already have, keyed
   `(migration, content-digest, action)`, so a retry skips the completed `delete`.
4. **Fail closed on a selected-but-unanchored migration.** A migration selected
   by version whose `stage` anchor matches no real stage by run-end must be a
   **terminal `MigrationStageNotFound`** with `status.version` NOT advancing —
   never silently dropped. Reconcile-end invariant: *every pending migration ends
   up executed or terminally failed.* (See the stage-anchor problem below.)
5. **Fail closed on an unresolved/unverified source.** Source not `Ready`, or
   (when required) not `SourceVerified` → transient requeue, do **not** advance
   `status.version`. Never read "couldn't load/trust the ladder" as "none needed."
6. **Single writer.** The controller's leader election already gives one reconciler
   per StageSet; that is the single-writer gate at the k8s level. For defense in
   depth against a failover overlap or an external runner, the `job`/`http` action
   path may additionally take a **DB advisory lock** (Liquibase/golang-migrate
   pattern). State this explicitly; don't rely on it being obvious.
7. **Sticky dirty/needs-intervention state.** A ladder that fails mid-run halts
   auto-progress under a distinct terminal reason (sibling to `MigrationArtifactInvalid`)
   and requires explicit acknowledgement (golang-migrate `dirty`+`force`, Flyway
   `repair`) rather than hot-looping — fits the existing no-backoff + slow-requeue
   convention for non-transient errors.

### The stage-anchor problem (load-bearing)

A shared ladder bakes in a concrete `stage` string, but consuming StageSets may
name their stages differently. Selection is version-only, so a mismatched anchor
selects the migration into `status.pendingMigrations` yet `forStage` never matches
— today that silently no-ops *and the version still advances*. Fix:

- Make the anchor **late-binding**: a migration's `stage` is optional and defaults
  to "before the first stage"; a stage may declare an **anchor alias**
  (`stage.migrationAnchor: "db-pre"`) that migrations reference semantically,
  decoupling the shared ladder from consumer-chosen stage names.
- Combine with guardrail 4: an anchor that resolves to nothing is a hard failure,
  not a skip.

**Ordering caveat (inherent to anchoring).** Execution interleaves migrations into
the stage loop, so *stage order can dominate version order across stages*: a
`to: 3.0.0` migration anchored to an early stage runs before a `to: 2.0.0`
migration anchored to a late stage. This is a property of *anchoring*, not of where
the ladder is stored — per-stage placement would hide it, not fix it. Within a
single anchor, `to`-order holds. Document it so authors anchor with their stage
order in mind and never assume pure global version ordering; if a later-version
migration must run before an earlier-version one, that is the author's anchoring
choice to make explicit.

## Security requirements (production MUST meet)

Threat model: a StageSet sources a destructive, ordered action ladder
(patch/apply/delete/job/http) from a registry/repo and runs it under the tenant
SA — multi-tenant, with external authors. `(NEW)` = new work; `(EXISTING)` = wire
an already-hardened mechanism onto the migration path.

1. **(NEW, CRITICAL) Provenance verification.** Sourcing destructive instructions
   is remote-controlled privileged execution, and the repo has **no signature
   verification today** — the fetcher checks sha256 against `status.artifact.digest`
   (integrity of bytes), not *who produced them*. Require/strongly-recommend
   `OCIRepository.spec.verify` (cosign keyless via Fulcio/Rekor + `matchOIDCIdentity`,
   or notation) for migration sources and **gate execution on `SourceVerified=True`**
   the same way it gates on `Ready=True`. Threat is asymmetric: a tampered
   Deployment rolls back; a tampered `DROP` is irreversible.
2. **(NEW, CRITICAL) Enforce digest pinning** for migration sources (or fail-closed/
   Warning on a tag-pinned migration source). Digest stops tag-swap; signature
   stops compromised-bytes-under-a-new-digest. Both required. *(Bucket sources have
   no native verify → restrict migration sources to OCI/Git, or enforce a
   controller-side check for buckets.)*
3. **(EXISTING) Same-namespace-only `migrationsSourceRef` by default** (force
   `Resolver.NoCrossNamespace` for this field). Cross-namespace destructive
   sourcing is opt-in and platform-gated — closes the confused-deputy vector the
   `ClusterStageMigration` rejection was about.
4. **(NEW) Restrict `spec.serviceAccountName`** via webhook (same namespace;
   optional naming convention) so a StageSet can't name a more-privileged SA. The
   TokenRequest mint path is already least-privilege; bound *which* identity it mints.
   **Decision: deferred / out of scope for sourced migrations.** `serviceAccountName`
   governs *all* of a StageSet's actions (stages and migrations alike), not the
   migration source specifically — restricting which SA a tenant may name is a
   general StageSet multi-tenancy/admission-policy concern best handled by cluster
   policy (Kyverno/Gatekeeper) or a future StageSet-wide control, not bolted onto
   the migration feature. The migration-specific blast-radius controls
   (same-namespace source, signature/pinning gates, action-host allowlist) stand on
   their own.
5. **(EXISTING) Reuse the SSRF-guarded fetcher + dial-time IP pin + size/decompression
   caps + tar-path validation** for the migration artifact and any `job`/`apply`
   sub-artifacts. All four byte-caps and redirect re-validation apply unchanged.
6. **(EXISTING + gate) Require a non-empty `--allowed-action-hosts`** when migration
   sourcing is enabled. The denylist deliberately permits RFC1918/in-cluster, so
   "allow-all minus special ranges" is too loose for *remote-authored* `http`
   actions — this is also the break in the **secret-exfil chain** (malicious
   ladder + `headersFrom` secret + loose host = credential exfil).
7. **(EXISTING + decision) Decide SOPS-on-migration-artifact explicitly.** If
   supported, thread the tenant-SA-scoped `buildDecryptor`/`decryptFiles` through
   the migration fetch. **Forbid plaintext secrets in the artifact**; secrets reach
   actions only via `bodyFrom`/`headersFrom` Secret references.
8. **(NEW) Scrub secret-shaped detail from Events/status** across all action verbs
   (the `http` body snippet is already bounded at 512 B; extend to `patch`/`job`/
   `delete` error surfaces). Status/Events are broadly readable.
9. **(NEW) Ledger integrity.** `status` must be a true subresource and the
   least-privilege tenant role must **not** grant `stagesets/status` write — else a
   tenant can pre-seed `executedMigrations` to **skip** a `DROP`, or remove an
   entry to **replay** one. Guardrail-2 digest-keying makes a forged/drifted entry
   fail safe toward *running* rather than skipping.
10. **(NEW) Self-contained audit trail.** Registry-stored content means the k8s
    audit log no longer shows *what changed*. Record the source revision + artifact
    digest + per-migration content-digest in status/Events at execution time, so
    "what ran" is reconstructable independent of the now-mutable registry; the
    signature supplies signer identity.
11. **(EXISTING + small NEW) DoS caps.** Fetcher byte-caps bound memory; add a
    parse-time cap on actions-per-ladder / migrations-per-ladder and bound action
    `Retries`, surfaced as `MigrationArtifactInvalid`.

## UX & observability requirements

1. **Rich pending/applied status, not `[]string`.** `status.pendingMigrations`
   becomes `[]PendingMigration{name, to, from, stage, actionVerbs[], digest}` and
   `status` records applied digests. `kubectl describe` and the CLI then show *what
   destructive thing runs, where, at which boundary* without reading `spec.migrations`.
2. **`stageset diff` must resolve the source.** Today `pendingMigrations`
   (`internal/cli/diff.go`) joins names against `ss.Spec.Migrations`, which is empty
   for a sourced ladder → the pre-flight shows empty fields for the destructive
   work. The diff must fetch+resolve the ladder and render full detail incl. verbs.
3. **`stageset lint-migrations` / `--migrations-file`** reusing the *exact*
   `validateMigrations` logic (semver parse of every `to`, constraint parse of every
   `from`, one-verb-per-action, unique non-empty action names) plus a "given
   from→to, which fire and which are excluded and why" report. **Extract and share
   the validator** between admission, reconcile, and CLI so they can't drift. Also
   publish a JSON Schema for the artifact for generic CI.
4. **`from`/`to` semver clarity.** `from: "1.0.0"` is a *constraint* meaning exactly
   `=1.0.0` — an author expecting "from 1.0.0 onward" gets a **silent skip**.
   Normalize/validate a bare `from` to `>=` (or reject it with a clear message), and
   surface "in range but excluded by `from`" as an Event, not a silent `continue`.
5. **Distinct failure reason + runbook.** A failed migration currently reuses
   `ReasonStageFailed`. Add `MigrationFailed` naming the migration, the failed
   action, and the recovery path; add `docs/runbooks/<reason>.md` pages (drift-gated)
   for every new reason.
6. **Documented skip/recover.** A wedged migration needs an annotation-driven
   skip/force (like the existing `stages.metio.wtf/reconcile-stage` token), not raw
   `status` editing.
7. **Baseline acknowledgement.** First-adoption baselining records the version and
   runs nothing — fine for greenfield, dangerous for an existing DB. Emit a distinct
   Event ("baselined at X; migrations ≤X assumed already applied") and consider an
   explicit `spec.version.assumeBaseline` so it's a deliberate act.
8. **Warn on tag-pinned migration sources** (resolved from open question to: yes).
   Auto-rolling destructive content on the source's reconcile cadence must be a
   knowing opt-in.
9. **Optional strict laddering** (`requireMigrationCoverage`): fail closed if a
   declared/major boundary is crossed with zero migrations selected, turning "forgot
   to author" from silent into `Ready=False`. At minimum emit an Event on "advanced
   N→M with zero migrations."

## Approval gate (later phase, but design for it)

Both dedicated operators pause destructive changes for human approval (Atlas
`ApprovalPending` + `approvalUrl`; SchemaHero `approve migration`). Signature
verification proves *origin*, not *intent* — complementary. A future
`spec.migrations*.requireApproval` (or an annotation token) would hold a destructive
transition at a `MigrationApprovalPending` reason until acknowledged. Park for a
later phase; reserve the reason name.

## New wire-stable reasons

`MigrationArtifactInvalid`, `MigrationStageNotFound`, `MigrationFailed`,
`MigrationDirty` (sticky needs-intervention), and reserved `MigrationApprovalPending`.
Reuse `SourceNotReady`; add a `SourceNotVerified`-style gate (or reuse Flux's
verification condition). Each new reason needs a `docs/runbooks/` page (the
`conditions_test` drift gate enforces one page per reason).

## Schema sketch

```go
type MigrationsSource struct {
    SourceRef meta.NamespacedObjectReference `json:"sourceRef"`          // OCIRepository/GitRepository/(Bucket)/ExternalArtifact, same-namespace by default
    Path      string                         `json:"path,omitempty"`     // file or directory within the artifact
    // Verification/digest requirements enforced via the source CR's .spec.verify + status.
}
// Migration type unchanged except: `stage` becomes optional (late-binding anchor),
// resolvable against a stage's `migrationAnchor` alias.
```

Webhook: reject both `spec.migrations` and `spec.migrationsSourceRef`; keep
"migrations require `spec.version`"; the per-migration checks move to
fetch/parse-time for sourced ladders (and run in the shared linter).

## Phasing (safety first — corrected)

1. **Safe core.** `migrationsSourceRef` fetch (reusing the SSRF-guarded fetcher,
   caps, revision pinning) + **all correctness guardrails 1–7** (incl. `(name,digest)`
   ledger, intra-migration per-action ledger, fail-closed-on-unanchored, late-binding
   anchor, dirty terminal state) + **security 1–6** (signature verify gate, digest
   pinning, same-namespace default, SA restriction, allowed-action-hosts) +
   `MigrationArtifactInvalid`/`MigrationStageNotFound`/`MigrationFailed`/`MigrationDirty`.
   *Do not ship sourced destructive execution without these.*
2. **Visibility.** Rich `status.pendingMigrations`, source-resolving `stageset diff`,
   shared `validateMigrations` + `stageset lint-migrations` + JSON Schema, audit-digest
   recording, secret scrubbing, baseline acknowledgement, tag-pin warnings.
3. **Ergonomics & policy.** JsonnetSnippet/ExternalArtifact tutorial, directory
   artifacts with an `atlas.sum`-style two-layer integrity manifest, `from`/`to`
   normalization, strict laddering, the approval gate.

## Non-goals

- Down-migrations / reversal (downgrades stay refused).
- Cluster-scoped sharing (the registry artifact provides cross-namespace sharing
  without a cluster object; revisit additively only if a single in-cluster object
  is ever proven necessary).

## Open questions

- Directory-of-files vs single-list artifact — support both, but require an
  integrity sum for the directory form (Atlas `atlas.sum` two-layer hash).
- Is signature verification **required** or **strongly recommended** for migration
  sources? (Leaning required for OCI; Git via GPG; Bucket disallowed or digest-only.)
- DB advisory lock as defense-in-depth (guardrail 6) — opt-in per action, or rely
  on controller leader-election alone for v1?

## Sources

Helm hooks/provenance; Argo CD sync phases & waves; Flux `dependsOn` + `OCIRepository.spec.verify` (and the unimplemented `flux#3264`); Atlas Operator (`AtlasMigration`, registry, dev-DB, dirty/baseline, pre-approval, `atlas.sum`); SchemaHero plan→approve; Flyway/Liquibase/golang-migrate checksum-ledger/baseline/lock conventions. (Full URL list in the review notes; key ones: helm.sh/docs/topics/charts_hooks, argo-cd sync-waves, fluxcd.io/flux/components/source/ocirepositories #verify, atlasgo.io/integrations/kubernetes/operator + /concepts/migration-directory-integrity.)
