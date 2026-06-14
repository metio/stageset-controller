# StageSet runbooks

One page per wire-stable `Reason` the controller sets on the `Ready` condition of a `StageSet`. Pages target operators reading `kubectl describe stageset` output: symptom, cause, diagnosis, remediation.

Point the controller at a published copy of these pages with `--runbook-base-url` (e.g. `--runbook-base-url=https://github.com/metio/flux-stageset-controller/blob/main/docs/runbooks`). The reason is then appended to each actionable Ready message as `(runbook: <base>/<reason>.md)`. Healthy reasons (`Succeeded`, `Suspended`) get no link.

| Reason | Page | Meaning |
|---|---|---|
| `Succeeded` (healthy) | [succeeded.md](succeeded.md) | every stage applied and verified |
| `Suspended` (intentional) | [suspended.md](suspended.md) | `spec.suspend` is set |
| `InvalidSpec` | [invalidspec.md](invalidspec.md) | the spec is rejected by validation |
| `SourceNotReady` | [sourcenotready.md](sourcenotready.md) | a stage's artifact has not been published yet |
| `ArtifactNotFound` | [artifactnotfound.md](artifactnotfound.md) | a stage's `sourceRef` resolves to no artifact |
| `ResolveFailed` | [resolvefailed.md](resolvefailed.md) | a stage's `sourceRef` could not be resolved |
| `DependencyNotReady` | [dependencynotready.md](dependencynotready.md) | a `dependsOn` StageSet is not Ready |
| `Stalled` | [stalled.md](stalled.md) | a terminal condition needing a spec change |
| `InvalidVersion` | [invalidversion.md](invalidversion.md) | the version file is missing or unparseable |
| `DowngradeRequiresMigration` | [downgraderequiresmigration.md](downgraderequiresmigration.md) | desired version is below the deployed one |
| `PreviousRevisionUnavailable` | [previousrevisionunavailable.md](previousrevisionunavailable.md) | rollback can't restore a GC'd revision |
| `UpdateDeferred` | [updatedeferred.md](updatedeferred.md) | a rollout is held by an update window |
| `StageFailed` | [stagefailed.md](stagefailed.md) | a stage failed to fetch, build, apply, or verify |
