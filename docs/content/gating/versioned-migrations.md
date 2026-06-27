---
title: Versioned migrations
description: Run migrations once, when the deployed version crosses a release boundary — inline, or sourced from a Flux artifact and shared across StageSets.
tags: [migrations, versioning, actions, sources, security]
---

Some changes only need to happen once, when you cross a release boundary — a
one-time data backfill on the way to 2.0, a schema conversion between 1.x and 2.x.
Versioned migrations run a ladder of [actions](/defining-a-release/actions/) exactly when the
deployed version steps over the boundary, and never again.

Versioning is off until you set `spec.version`.

## Declaring the version

The controller needs to know *what version is currently being deployed*. There are
three ways to declare it; pick by **where the version lives**.

| Source | The version lives… | Best for |
|---|---|---|
| [`version.value`](#inline--versionvalue) | on the `StageSet` | environment-pinned versions, quick starts |
| [`version.fromObject`](#from-a-rendered-object--versionfromobject) | inside the manifests | **any source, including JaaS** — the recommended default |
| [`version.fromArtifact`](#from-a-file-in-the-artifact--versionfromartifact) | a file in the artifact | Git/OCI/Bucket sources that can ship a `VERSION` file |

Whichever you choose, the resolved value is trimmed and parsed as semver (a leading
`v` is accepted). A missing stage/object/file, an empty value, or an unparseable
one fails terminally with the `InvalidVersion` reason (see its
[runbook](/runbooks/invalidversion/)) — a half-versioned system is worse than an
unversioned one.

### Inline — `version.value`

The `StageSet` author pins the version directly. Use this when the version is a
property of the environment rather than of the content, or to get started quickly:

```yaml
spec:
  version:
    value: "2.1.0"      # bump this when you cut a release
```

The trade-off: the version is declared here, not carried by the content, so you
bump it by editing the `StageSet`.

### From a rendered object — `version.fromObject`

The recommended way to let the version travel with the content.
[Kubernetes](https://kubernetes.io/docs/) has a standard place for an
application's version: the `app.kubernetes.io/version` label. Well-formed manifests
set it, so the version is already inside the manifests — `fromObject` reads it back.
This works for every source kind, including a single-document renderer like
[JaaS](https://jaas.projects.metio.wtf/) that has no room for a separate file.

A StageSet has **one** version that all its stages converge on; `fromObject` only
chooses *which* stage's rendered output to read it from. That stage is optional —
omit it to read from the first (leading) stage, which carries the new version
first.

```yaml
spec:
  version:
    fromObject:
      # stage omitted → defaults to the first stage (here, app)
      kind: Deployment      # the object to read
      name: web
      # fieldPath omitted → reads metadata.labels['app.kubernetes.io/version']
  stages:
    - name: app
      sourceRef:
        name: my-app
```

The controller builds the `app` stage's manifests (the same render it applies),
finds the `Deployment/web` object, and reads its `app.kubernetes.io/version` label.
Because the label is part of the manifests, the version changes in lockstep with
the content — no second file to keep in sync.

**Reading a different field.** Set `fieldPath` to a kubectl-style JSONPath that
resolves to the bare version string. (It must be the version *only*; a JSONPath
can't split an `image: web:2.1.0` value, so prefer the label.) `apiVersion` is
optional and narrows the match when a `Kind`+`Name` pair would be ambiguous:

```yaml
spec:
  version:
    fromObject:
      stage: app
      apiVersion: v1
      kind: ConfigMap
      name: app-meta
      fieldPath: "{.data.version}"   # must resolve to a bare semver, e.g. 2.1.0
```

This is the path the [Jsonnet-to-rollout tutorial](/get-started/jsonnet-to-rollout/)
uses: the snippet renders the version into the manifest's version label, and the
StageSet reads it straight back.

### From a file in the artifact — `version.fromArtifact`

The version travels with the content as a **dedicated file** containing a single
semver. This fits **Git/OCI/Bucket** sources, where you can ship an extra file
beside the manifests. (It does *not* fit JaaS `rendered` output, which is a single
`rendered.json`; use `fromObject` there.)

**Who writes it, and where:** the artifact's producer. For a Git source, commit a
`VERSION` file in the repo; for an OCI/Bucket artifact, include it in the pushed
tree. The file lives at `path` inside the named stage's artifact, relative to the
artifact root:

```text
# VERSION — committed alongside the manifests it versions
2.1.0
```

```yaml
spec:
  version:
    fromArtifact:
      stage: app          # which stage's artifact carries the file
      path: VERSION       # the file's path inside that artifact (cleaned; no leading ./)
  stages:
    - name: app
      sourceRef:
        kind: GitRepository
        name: my-app
```

The controller fetches the `app` stage's artifact and reads the file at `path`.

## Declaring migrations

Each migration names the boundary it crosses (`to`, optionally `from`), the stage
it anchors before, and the actions to run:

```yaml
spec:
  version:
    fromArtifact:
      stage: app
      path: VERSION
  migrations:
    - name: backfill-ledger-2-0
      from: "1.*"               # optional: only when coming from a 1.x
      to:   "2.0.0"             # the boundary this migration crosses
      stage: app               # runs before this stage's pre-actions
      actions:
        - name: backfill
          job:
            sourceRef:
              name: ledger-backfill-job
  stages:
    - name: app
      sourceRef:
        name: my-app
```

`to` is an **exact** target version, the same kind of value `version.value`
holds. `from` is a semver **constraint**, not an exact version: it is matched
against the currently deployed version with the same grammar Helm and
`flux`-style ranges use. So `from` accepts ranges like `>=1.0.0, <2.0.0`,
`1.x`, or `^1.2` — the migration fires only when the deployed version satisfies
that constraint *and* the run crosses up to `to`. A `from` of `>=1.0.0, <2.0.0`
with `to: "2.0.0"` runs the migration on any 1.x → 2.0.0 transition; omit
`from` to fire on every crossing up to `to` regardless of where you start.

When the deployed version crosses from a `1.x` into `2.0.0`, the `backfill` job
runs once, anchored before the `app` stage. The controller tracks progress so a
retry doesn't re-run a completed migration:

- `status.version` — the deployed version, written only after a fully successful
  run.
- `status.pendingMigrations` — migrations the next run will execute.
- `status.executedMigrations` / `status.executedMigrationActions` — the in-flight
  ledgers (per migration, and per action within a migration) for the current
  transition, cleared once the version advances.

Migrations emit `MigrationStarted` / `MigrationCompleted` events. A downgrade that
would skip a required migration is refused with the `DowngradeRequiresMigration`
reason — see its [runbook](/runbooks/downgraderequiresmigration/). For the
failure path, see [When a migration fails](#when-a-migration-fails).

**First adoption (baselining).** The first time a StageSet sets `spec.version` and
has no `status.version` yet, the controller records the version and runs **no**
migrations — an existing deployment is assumed to already be at that version. It
emits a `MigrationsBaselined` event so you can tell this apart from a real no-op
and confirm the deployment really is at the recorded version. Migrations run only
on a subsequent transition that crosses their boundary.

## Sharing one ladder across StageSets

Deploying the same application into many namespaces — one `StageSet` each — means
the same migration ladder in every one. Copying a destructive ladder into N specs
and keeping them in sync is exactly where a missed edit becomes a data-loss
incident. Author the ladder **once** as an artifact and point every StageSet at it
with `migrationsSourceRef` instead:

```yaml
spec:
  version:
    fromObject:
      stage: app
      kind: Deployment
      name: web
  migrationsSourceRef:
    sourceRef:
      kind: OCIRepository
      name: orders-migrations
    path: migrations          # optional: a file or directory within the artifact
  stages:
    - name: app
      sourceRef:
        name: my-app
```

`migrationsSourceRef` is **mutually exclusive** with the inline `spec.migrations`.
The artifact holds the ladder as YAML or JSON — each `.yaml`, `.yml`, or `.json`
file is one migration or a list of them, parsed strictly so a misspelled field is
rejected rather than ignored. The ladder lives once in the registry; each
namespace carries only a small Flux source plus an identical StageSet, and a new
release is a push to the registry, not an edit to N specs.

Sourcing reuses the same fetch path as stage sources — revision pinning, the SSRF
guard, and the size caps all apply. A source that is not yet ready holds the
transition (the version is not advanced) rather than running a half-loaded ladder;
a malformed or oversized artifact fails terminally with the
`MigrationArtifactInvalid` reason — see its
[runbook](/runbooks/migrationartifactinvalid/).

### Anchoring a shared ladder

A migration's `stage` says *where in the rollout it runs* — before that stage's
pre-actions. A ladder shared across StageSets can't hard-code one StageSet's stage
names, so give each stage an **anchor role** and let migrations reference the role:

```yaml
spec:
  stages:
    - name: prepare
      migrationAnchor: db-pre     # this stage plays the "db-pre" role
      sourceRef:
        name: my-app
    - name: rollout
      sourceRef:
        name: my-app
```

A migration in the shared ladder anchors to `db-pre` and resolves to whichever
stage declares that role — `prepare` here, `db-migrate` in another StageSet — so
one ladder travels across differently-named stages. A migration's `stage` resolves
to the stage whose `migrationAnchor` matches, then by stage `name`; omit it to
anchor before the first stage. Anchor keys are unique across names and roles. A
migration that resolves to no stage fails closed with `MigrationStageNotFound`
(see its [runbook](/runbooks/migrationstagenotfound/)) — never silently skipped.

## Verifying and pinning the source

A sourced ladder runs remote-authored, destructive instructions, so the controller
can require the source prove its provenance and immutability before running:

- **Signature verification.** Configure `spec.verify` (cosign or notation) on the
  Flux source. A source whose verification *fails* is always refused
  (`MigrationSourceNotVerified`). The controller flag
  `--require-verified-migration-sources` additionally refuses a source that
  configures no verification at all. Off by default; recommended in production.
- **Immutable pinning.** Pin the source to a digest (`OCIRepository`
  `spec.ref.digest`) or commit (`GitRepository` `spec.ref.commit`) so a tag
  overwrite can't auto-roll new destructive content. A mutable-pinned source emits
  a `Warning` event; `--require-pinned-migration-sources` makes it a hard refusal
  (`MigrationSourceNotPinned`). Off by default; recommended in production.

```yaml
apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata:
  name: orders-migrations
spec:
  ref:
    digest: sha256:…            # immutable; bump deliberately to ship new migrations
  verify:
    provider: cosign
    matchOIDCIdentity:
      - issuer: https://token.actions.githubusercontent.com
        subject: ^https://github.com/your-org/.+$
  # url, interval, …
```

Two further guards apply to a sourced ladder: it is always resolved in the
StageSet's own namespace (a cross-namespace `migrationsSourceRef` is rejected), and
an `http` action requires the controller's `--allowed-action-hosts` allowlist to be
set, so remote-authored calls cannot reach arbitrary in-cluster endpoints.

## When a migration fails

A failed migration halts the run at its anchoring stage under the
`MigrationFailed` reason — distinct from a stage's own `StageFailed` — then retries
with backoff. Retries are safe: the per-action ledger records each completed
action keyed by the migration's content digest, so a retry resumes at the failed
action rather than re-running a destructive one.

After repeated failures the migration escalates to `MigrationDirty`: the controller
stops auto-retrying destructive work against an uncertain state. Fix the cause,
then clear the halt with a manual reconcile:

```shell
kubectl annotate stageset orders \
  reconcile.fluxcd.io/requestedAt="$(date +%s)" --overwrite
```

See the [`MigrationFailed`](/runbooks/migrationfailed/) and
[`MigrationDirty`](/runbooks/migrationdirty/) runbooks.
