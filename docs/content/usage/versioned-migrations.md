---
title: Versioned migrations
description: Run migrations once, when the deployed version crosses a release boundary.
tags: [migrations, versioning, actions]
---

Some changes only need to happen once, when you cross a release boundary — a
one-time data backfill on the way to 2.0, a schema conversion between 1.x and 2.x.
Versioned migrations run a ladder of [actions](/usage/actions/) exactly when the
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

```yaml
spec:
  version:
    fromObject:
      stage: app            # which stage's rendered manifests carry it
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

This is the path the [Jsonnet-to-rollout tutorial](/tutorials/jsonnet-to-rollout/)
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
- `status.executedMigrations` — the in-flight ledger for the current transition.

Migrations emit `MigrationStarted` / `MigrationCompleted` events (and
`MigrationFailed` on error). A downgrade that would skip a required migration is
refused with the `DowngradeRequiresMigration` reason — see its
[runbook](/runbooks/downgraderequiresmigration/).
