---
title: MigrationSourceNotVerified
description: A sourced migration ladder's source is not signature-verified; the destructive ladder is refused until verification passes.
tags: [runbooks, migrations, security, sources, troubleshooting]
---

## Symptom

`READY=False`, `REASON=MigrationSourceNotVerified`. The Message says either
verification failed (`SourceVerified=False`) or that verification is required but
not configured. `status.version` is **not** advanced — the migration ladder does
not run.

## Cause

Sourcing a migration ladder from `spec.migrationsSourceRef` runs remote-authored,
destructive instructions, so the controller can require they be provenance-verified
before executing. Verification state comes from the source CR's
`status.conditions[SourceVerified]`, which Flux source-controller sets from the
source's `spec.verify` (cosign or notation):

- **`SourceVerified=False`** — the source configured `spec.verify`, but the
  artifact's signature did not pass. Always refused, regardless of flags — a
  tampered or unsigned-by-the-expected-identity artifact must not run.
- **No `SourceVerified` condition + `--require-verified-migration-sources`** — the
  controller requires verification, but the source configures none.

## Fix

- Configure signature verification on the source. For an `OCIRepository`:

  ```yaml
  apiVersion: source.toolkit.fluxcd.io/v1
  kind: OCIRepository
  metadata:
    name: orders-migrations
  spec:
    verify:
      provider: cosign
      matchOIDCIdentity:
        - issuer: https://token.actions.githubusercontent.com
          subject: ^https://github.com/your-org/.+$
    # ...
  ```

  Once source-controller sets `SourceVerified=True`, the ladder runs on the next
  reconcile.

- If verification genuinely failed, the published artifact is not signed by the
  expected identity — investigate the publishing pipeline before trusting it; do
  not work around it by disabling verification for a destructive ladder.

- `--require-verified-migration-sources` is off by default but strongly
  recommended in production; a source whose verification *fails* is refused even
  with the flag off.
