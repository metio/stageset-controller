# Reason: InvalidVersion

## Symptom

`READY=False`, `REASON=InvalidVersion`. Terminal: the run does not requeue until the spec or the version file is fixed.

## Cause

`spec.version.fromArtifact` names a stage and a path to a file holding a single semver string, but that file is missing from the artifact or does not parse as a semantic version. The controller refuses to proceed rather than deploy a half-versioned system — a system whose recorded version is unknown is worse for migrations than an unversioned one.

Common triggers:

- a typo in `spec.version.fromArtifact.path`
- the wrong `spec.version.fromArtifact.stage` (a stage whose artifact doesn't carry the version file)
- the version file contains extra whitespace, a `v` prefix the parser rejects, or non-semver text

## Diagnosis

```shell
kubectl describe stageset <name> -n <namespace>   # Message names the stage + path
```

Inspect the artifact the stage resolves to and confirm the file exists and contains a bare semver (e.g. `2.1.0`).

## Remediation

- Fix the `path`/`stage` to point at the real version file, or
- Correct the file's contents to a parseable semver, or
- If you don't need versioned migrations, remove `spec.version` entirely (this disables versioning and migrations).
