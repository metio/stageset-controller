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
stagesetctl plan [NAME...] [flags]
```

Plan one StageSet by name, several by name, or a whole fleet with
`--all-namespaces` / `--selector`; `-o json|yaml` emits the plan as data for a
CI step or a PR comment.

| Flag | Default | Description |
|---|---|---|
| `--stage` | _(all)_ | Limit the plan to these stages (repeatable). |
| `--source-dir` | _(none)_ | `stage=DIR` to render a stage from a local directory instead of its source (repeatable). |
| `--output`, `-o` | `text` | Output format: `text`, `json`, or `yaml`. |
| `--all-namespaces`, `-A` | `false` | Plan every StageSet across all namespaces. |
| `--selector`, `-l` | _(none)_ | Plan StageSets matching a label selector. |

Name arguments select those StageSets by name; with no name, `--all-namespaces`
and `--selector` choose the set to plan (a bare `plan` plans every StageSet in the
namespace). Combining a name with either flag is a usage error.

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
  prunes:
    ⚠ PersistentVolumeClaim/moodle-cache  (stage app) — deleting this destroys its data
  note: scope: Lifetime results reflect the current cluster (completionAnchor witnesses are read live).
```

The `migrations` block lists the version-boundary migrations the controller has
queued; the `gates` block lists what would hold the rollout — a closed update
window (recomputed now, a pure function of the schedule and the clock), an
[error-budget](/gating/error-budget/) freeze, or a stage
[awaiting promotion](/gating/stage-promotion/) — each tagged `[live: now]` or
`[live: status]` to say whether it was recomputed or read from status. The
`prunes` block lists objects a stage would delete — in its
[inventory](/api/stageinventory/) but no longer in its render — and flags a
state-bearing one (a PVC, a StatefulSet) with `⚠`, so a prune that would destroy
data is a red line in the plan rather than an incident after the fact.

## Previewing a rollback

When the desired version is below the deployed version, the plan is a rollback
safety check: a `rollback` block replaces `migrations`, listing which
[migration](/gating/versioned-migrations/#rolling-back) boundaries the downgrade
would reverse and flagging any that cannot be reversed.

```text
$ stagesetctl plan moodle -n moodle-acme
StageSet moodle-acme/moodle  (version 1.2.0 → 1.0.0, downgrade)
  stage app  (revision sha256:abc123…)
  rollback:
    reverse  schema-1-1           (to 1.1.0)  [job]
    ⚠ irreversible  schema-1-2    (to 1.2.0) — no down actions; downgrade refused
```

For an inline ladder the steps are computed the same way the controller selects
them, so the preview is reproducible from the spec. An irreversible boundary — one
that declares no `down` actions — is the red line: a downgrade that cannot be done
safely is caught before merge, not after. A downgrade with `spec.version.allowDowngrade`
unset is noted as not enabled.

The version is resolved from the source the same way the controller resolves it:
an inline `spec.version.value`, a field of a rendered object
(`spec.version.fromObject`), or a version file in a stage's artifact
(`spec.version.fromArtifact`) — each reproducible from the source, so
version-scoped verdicts are exact.

## Planning a fleet

With `--all-namespaces` or `--selector`, `plan` covers many StageSets at once and
`-o json` turns the result into data — one array element per StageSet, each with
its stages, action verdicts, migrations, gates, and prunes — for a CI gate or a
PR comment across the fleet:

```text
stagesetctl plan --all-namespaces -o json
```

The exit code aggregates: **3** if any StageSet could not be planned, otherwise
**1** if any would run something, **0** if none would. A StageSet that fails to
plan — an unreadable decryption key, a source that will not render — reports its
`error` and the rest are still planned, so one broken tenant never hides the fleet.
