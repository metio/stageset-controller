---
title: MigrationStageNotFound
description: A version-selected migration anchors to a stage that doesn't exist in this StageSet; the version is held back rather than skipping the migration.
tags: [runbooks, migrations, stages, troubleshooting]
---

## Symptom

`READY=False`, `REASON=MigrationStageNotFound`. The Message names the migration
and the anchor value it could not resolve. `status.version` is **not** advanced —
the transition is held until the anchor resolves.

## Cause

A migration that the version transition selected (`current < to <= desired`)
anchors to a stage that this StageSet does not have. A migration's `stage` value
resolves to the stage whose `migrationAnchor` (preferred) or `name` equals it;
an empty `stage` anchors before the first stage. If none matches, the controller
**fails closed** instead of silently never running the migration — the failure
mode a shared, late-binding ladder must not have (a `DROP` that quietly never
runs while the app advances to a schema that needs it).

Common causes:

- A **shared ladder** (sourced via `spec.migrationsSourceRef`) anchors to a role
  like `db-pre`, but this StageSet's stages don't declare
  `migrationAnchor: db-pre`.
- A typo in a migration's `stage` value, or a stage was renamed without updating
  the anchor.

## Fix

- If the migration should run before a particular stage, declare that role on
  the stage:

  ```yaml
  spec:
    stages:
      - name: prepare
        migrationAnchor: db-pre   # now `stage: db-pre` migrations anchor here
  ```

- Or point the migration's `stage` at an existing stage `name` (inline ladders),
  or leave it empty to anchor before the first stage.
- For a shared ladder, ensure every consuming StageSet declares the
  `migrationAnchor` roles the ladder references — that's how one ladder stays
  portable across StageSets whose stage names differ.

The condition clears and the version advances on the next reconcile once the
anchor resolves.
