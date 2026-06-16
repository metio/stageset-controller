# StageSet quick reference

The authoritative, current docs are at <https://stageset.projects.metio.wtf/>
(`/llms.txt` for a link index, `/llms-full.txt` for everything concatenated).
This is a compact cheat-sheet.

## spec fields (StageSet, `stages.metio.wtf/v1`)

- `interval` (optional — defaults to the controller's `--default-interval`)
- `retryInterval`, `driftDetectionInterval`, `timeout`, `suspend`
- `dependsOn: [{name, namespace}]` — ordering between StageSets
- `serviceAccountName` — impersonated for every cluster operation
- `kubeConfig.secretRef` — apply to a remote cluster
- `version.{fromArtifact|value}` + `migrations[]` — version-gated migrations
- `rollbackOnFailure` — restore last-good revision on failure
- `updateWindows[]` (`type: Allow|Deny`, cron `schedule`+`duration` or `from`/`to`,
  `timeZone`) + `windowScope: Updates|All`
- `stages[]` (required, ≥1):
  - `name` (required), `sourceRef` (required: `name`; `kind` defaults
    `ExternalArtifact`; also `GitRepository`/`OCIRepository`/`Bucket` or a producer)
  - `path`, `prune` (default true), `timeout`, `force`
  - `applyHelmHookResources`, `patches[]` (Kustomize), `postBuild.{substitute,
    substituteFrom}`
  - `conflictPolicy.{default, rules[]}` (`Fail|Recreate|KeepExisting`;
    `allowDataLoss` for PVC/PV Recreate)
  - `actions.{pre,post,onFailure}[]` — each Action has `name` + **exactly one** of
    `patch|http|wait|job|delete|apply`
  - `readyChecks.{checks[], exprs[], timeout, disableWait}`

`status` is controller-owned (conditions, per-stage phases, revisions, version,
pendingUpdate). Never author it.

## CLI (`stagesetctl`, also `kubectl stageset`)

- `get [NAME] [-A] [-o yaml|json]` — human-readable status
- `build NAME [--stage] [--source-dir [STAGE=]PATH] [--show-secrets] [--as-tenant]`
  — render manifests to stdout
- `diff NAME [...]` — server-side dry-run preview; exit 1 on changes (CI gate)
- `reconcile NAME [--stage] [--with-source] [--update-now] [--force] [--wait]`

## Runbooks

`status.conditions[Ready].reason` → `https://stageset.projects.metio.wtf/runbooks/<reason-lowercased>/`.
