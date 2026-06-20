---
title: stagesetctl lint-migrations
description: Validate a migration ladder file or directory before publishing it to a source.
tags: [cli, migrations, sources]
---

Validate a [migration ladder](/usage/versioned-migrations/) — a single file, or a
directory of `.yaml`/`.yml`/`.json` files — before publishing it to a source the
controller will fetch. It runs the **exact checks the controller runs** at
admission and at sourced-artifact fetch time, so a ladder that lints clean here
won't fail closed later in the cluster.

```text
stagesetctl lint-migrations PATH
```

It checks:

- strict parsing — a misspelled field (`acions:` for `actions:`) is rejected, not
  silently dropped;
- each migration has a name, a `to` that parses as a concrete semver, and an
  optional `from` that parses as a semver constraint;
- each action sets exactly one verb, with a unique, non-empty name;
- migration names are unique across the ladder.

On success it prints the parsed ladder — each migration's boundary, anchor stage,
and action verbs — so you can eyeball what will run:

```text
✓ 1 migration(s) valid
  drop-legacy  1.x → 2.0.0  before db-pre  delete
```

## Simulating a transition

The `from`/`to` selection math (`current < to <= desired` and the `from`
constraint matching `current`) is otherwise invisible until a reconcile runs.
Pass `--from` and `--to` to simulate a transition and see, using the controller's
own selection logic, which migrations fire and why the rest are excluded:

```text
$ stagesetctl lint-migrations migrations/ --from 1.2.0 --to 2.0.0
✓ 3 migration(s) valid
  …
transition 1.2.0 → 2.0.0:
  drop-legacy     FIRES     delete
  add-2-1         excluded  to 2.1.0 is not in the crossed range (1.2.0, 2.0.0]
  backfill        excluded  from ">=1.5.0" does not match current 1.2.0
```

This is the quickest way to catch a migration that silently won't fire — for
example a `from` constraint that excludes the version you're upgrading from.

## Exit codes

| Code | Meaning |
|---|---|
| `0` | the ladder is valid |
| `1` | a validation problem was found (printed to stdout) |
| `3` | the path could not be read |

Wire it into CI on the repository that holds the ladder, before the artifact is
published, so authoring problems surface in review rather than as a
`MigrationArtifactInvalid` status across every consuming StageSet. This command
needs no cluster connection.
