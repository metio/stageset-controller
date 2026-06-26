---
title: MigrationSourceNotPinned
description: A sourced migration ladder's source is pinned to a mutable tag/branch instead of an immutable digest/commit, and pinning is required.
tags: [runbooks, migrations, security, sources, troubleshooting]
---

## Symptom

`READY=False`, `REASON=MigrationSourceNotPinned`. The Message says the migrations
source is pinned to a mutable tag/branch and `--require-pinned-migration-sources`
is set. `status.version` is **not** advanced — the migration ladder does not run.

When the flag is *off*, this does not block: the ladder runs, but a
`Warning MigrationSourceMutable` event is emitted so the auto-rollout risk stays
visible.

## Cause

A migration ladder sourced from `spec.migrationsSourceRef` runs remote-authored,
destructive instructions. If the source is pinned to a **mutable** revision — an
`OCIRepository` `spec.ref.tag`/`spec.ref.semver`, or a `GitRepository`
`spec.ref.branch`/`tag`/`semver` — an upstream overwrite of that tag/branch
silently changes what runs on the next source reconcile. An immutable pin
(`OCIRepository` `spec.ref.digest` or `GitRepository` `spec.ref.commit`) removes
that auto-rollout window.

A `Bucket` source is never pinned; an in-cluster `ExternalArtifact` is exempt
(its producer owns the revision).

## Remediation

Pin the source to an immutable revision. For an `OCIRepository`:

```yaml
apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata:
  name: orders-migrations
spec:
  ref:
    digest: sha256:abc123...   # instead of tag: latest
  # ...
```

For a `GitRepository`, set `spec.ref.commit`. To roll new migrations, bump the
digest/commit in the source — a deliberate, reviewable change — rather than
overwriting a tag.

`--require-pinned-migration-sources` is off by default but recommended in
production for destructive ladders.
