---
title: MigrationArtifactInvalid
description: A migration ladder sourced via spec.migrationsSourceRef could not be parsed or failed content validation; the version is held back until the artifact is fixed.
tags: [runbooks, migrations, sources, troubleshooting]
---

## Symptom

`READY=False`, `REASON=MigrationArtifactInvalid`. The Message names the file and
the parse or validation error. `status.version` is **not** advanced — the
transition is held until the artifact is fixed and republished.

## Cause

The StageSet sources its migration ladder from `spec.migrationsSourceRef`, and
the fetched artifact could not be turned into a valid ladder. Each
`.yaml`/`.yml`/`.json` file in the artifact (under the optional `path`) must be a
YAML or JSON document that is either a list of migrations or a single migration.
Parsing is strict — a misspelled field is rejected rather than silently dropped,
because a destructive ladder must not run a half-understood definition.

Common causes:

- The artifact contains no `.yaml`/`.yml`/`.json` files (wrong `path`, or the
  source published something else).
- Malformed YAML/JSON, or a misspelled field (`acions:` for `actions:`).
- A migration with an empty `name` or `to`, an action that sets zero or more than
  one verb, an action with an empty or duplicate name, or two migrations sharing
  a name (names are the idempotency-ledger key and must be unique).

This validates the same invariants the admission webhook enforces for inline
`spec.migrations`; for a sourced ladder they can only be checked at fetch time,
not at `kubectl apply`.

## Remediation

- Fix the offending file in the source repository/registry and republish the
  artifact. Validate it the same way the controller does before publishing — the
  ladder format is one migration, or a list of them, per file:

  ```yaml
  - name: add-orders-table
    to: "2.0.0"
    stage: db-pre          # a stage name or a stage's migrationAnchor
    actions:
      - name: create-table
        apply: { ... }
  ```

- Check `spec.migrationsSourceRef.path` points at the file or directory that
  actually holds the ladder.

The condition clears and the version advances on the next reconcile once the
republished artifact parses and validates.
