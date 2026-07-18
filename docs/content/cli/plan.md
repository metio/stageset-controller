---
title: stagesetctl plan
description: Preview what the next reconcile will do — which actions run, skip, or re-run, and why.
tags: [cli, actions, operations]
---

Where [`diff`](/cli/diff/) shows which objects a reconcile would change, `plan`
shows what it would **do**: per stage, which `pre`/`post`
[actions](/defining-a-release/actions/) will run, skip, or re-run on the next
reconcile, and why. It renders each stage, resolves the version, reads the
[ledgers](/defining-a-release/actions/#scope-revision-version-or-lifetime), and
applies the same scope rules the controller gates on — so the preview agrees with
what the controller will do.

It reads the cluster (ledgers, `completionAnchor` witnesses) but changes nothing,
and it predicts what will be **attempted**, in what order, under which scope —
never whether an action will succeed.

```text
stagesetctl plan NAME [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--stage` | _(all)_ | Limit the plan to these stages (repeatable). |
| `--source-dir` | _(none)_ | `stage=DIR` to render a stage from a local directory instead of its source (repeatable). |

`plan` follows the [`diff`](/cli/diff/) exit-code convention, so it works as a
merge gate: **0** when the reconcile would run nothing, **1** when at least one
action would run, **2** for a usage error, **3** for a runtime failure.

## Example

```text
$ stagesetctl plan moodle -n moodle-acme
StageSet moodle-acme/moodle  (version 1.0.0 → 1.1.0)
  stage app  (revision sha256:abc123…)
    pre  maintenance-on     WILL RUN (scope: Version — new version episode 1.0.0 → 1.1.0)
    pre  db-upgrade         WILL RUN (scope: Version — new version episode 1.0.0 → 1.1.0)
    post install-database   SKIP     (scope: Lifetime — completed once, ever)
  migrations:
    schema-1-1               1.0.x → 1.1.0  before app  [job]
  gates:
    update window  HOLD  (closed; opens 2026-06-16T02:00:00Z) [live: now]
  note: scope: Lifetime results reflect the current cluster (completionAnchor witnesses are read live).
```

The `migrations` block lists the version-boundary migrations the controller has
queued; the `gates` block lists what would hold the rollout — a closed update
window (recomputed now, a pure function of the schedule and the clock), an
[error-budget](/gating/error-budget/) freeze, or a stage
[awaiting promotion](/gating/stage-promotion/) — each tagged `[live: now]` or
`[live: status]` to say whether it was recomputed or read from status.

A version resolved off the rendered manifests (an inline `spec.version.value` or
`spec.version.fromObject`) is reproducible from the source; a
`spec.version.fromArtifact` version is not yet resolved in the preview, so
version-scoped actions are shown as would-run.
