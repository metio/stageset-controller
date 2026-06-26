---
title: MigrationCoverageMissing
description: A major-version transition has no migration covering it, and requireMigrationCoverage is set; the version is held back.
tags: [runbooks, migrations, versioning, troubleshooting]
---

## Symptom

`READY=False`, `REASON=MigrationCoverageMissing`. The Message names the boundary
(e.g. `1.4.0 → 2.0.0`). `status.version` is **not** advanced.

## Cause

`spec.version.requireMigrationCoverage` is set, and the version transition crosses
a **major-version boundary** (the major component increased) with **no migration**
selected for it. A major bump usually implies breaking changes that need a
migration, so rather than advance silently the controller fails closed and asks
for an explicit decision.

Minor and patch transitions are never gated by this — only a major boundary.

## Remediation

Either add a migration that covers the boundary:

```yaml
spec:
  migrations:
    - name: upgrade-to-2
      to: "2.0.0"            # covers the 1.x → 2.0.0 major boundary
      from: ">=1.0.0"
      stage: app
      actions: [ ... ]
```

…or, if the major bump genuinely needs no migration, drop
`spec.version.requireMigrationCoverage` (or correct the version if the bump was
unintended). The condition clears on the next reconcile once a covering migration
exists or the flag is removed.
