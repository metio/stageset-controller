---
title: InvalidVersion
description: A version source or value could not be parsed as semver.
tags: [runbooks, versioning, troubleshooting]
---

## Symptom

`READY=False`, `REASON=InvalidVersion`. Terminal: the run does not requeue until the spec or the version file is fixed.

## Cause

A version `spec.version` (or a migration boundary) could not be resolved to a parseable [semver](https://semver.org/). The controller refuses to proceed rather than deploy a half-versioned system — a system whose recorded version is unknown is worse for migrations than an unversioned one. The Message names which input failed. By version source:

- **`spec.version.value`** — the inline string is not a semver.
- **`spec.version.fromObject`** — the named stage doesn't exist; the object (`kind`/`name`) isn't in the stage's rendered manifests; the `fieldPath` is invalid JSONPath or resolves to empty; or the value read (by default the `app.kubernetes.io/version` label) is missing or not a semver.
- **`spec.version.fromArtifact`** — the named stage doesn't exist; the file at `path` is missing from the stage's artifact, empty, or doesn't parse as a semver.
- **`spec.version` sets none** of `value`/`fromObject`/`fromArtifact`.
- A **migration's `to` or `from`** is not a valid semver.
- The recorded **`status.version`** is not a semver (corrupted status).

Common triggers across all of them: a `v` prefix or trailing whitespace the parser rejects, or non-semver text (e.g. a Git SHA or a `latest` tag) where a version was expected.

## Diagnosis

```shell
kubectl --namespace <namespace> describe stageset <name>   # Message names the failing input
kubectl --namespace <namespace> get stageset <name> --output jsonpath='{.spec.version}{"\n"}'
```

Then, depending on which source the Message names:

```shell
# fromObject: confirm the field carries a bare semver on the rendered object
stagesetctl --namespace <namespace> build <name> --stage <stage> | grep -i version

# fromArtifact: confirm the file exists and contains only a semver (e.g. 2.1.0)
# inspect the resolved artifact for the stage named in the Message
```

## Remediation

Match the failing input in the Message:

- **`value`** — correct the inline string to a bare semver (`2.1.0`, not `v2.1.0`).
- **`fromObject`** — fix the `stage`/`kind`/`name` to point at a real rendered object, fix the `fieldPath`, and ensure the field (default: `app.kubernetes.io/version` label) carries a semver.
- **`fromArtifact`** — fix `path`/`stage` to the real version file, or correct the file to a bare semver.
- **migration `to`/`from`** — correct the boundary to a valid semver.
- If you don't need [versioned migrations](/gating/versioned-migrations/), remove `spec.version` entirely (this disables versioning and migrations).
