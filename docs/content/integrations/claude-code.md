---
title: Claude Code skill
description: Install the StageSet plugin so Claude Code authors and operates StageSets.
tags: [contributing, claude-code, skill, plugin]
---

The repository ships a [Claude Code](https://claude.com/claude-code) skill that
teaches the assistant to author and operate `StageSet` resources: writing and
editing StageSet YAML, wiring a Flux source or a producer like
[JaaS](https://jaas.projects.metio.wtf/) into staged rollouts, adding typed
actions, ready checks, update windows, versioned migrations, and conflict
policies, and driving a StageSet with the `stagesetctl` CLI. With it installed,
Claude Code works against the real schema and defaults instead of guessing.

## Install the plugin

The skill is packaged as a plugin in a marketplace. Add the marketplace from the
repository, then install the plugin:

```text
/plugin marketplace add metio/stageset-controller
/plugin install stageset@stageset
```

The first command registers the `stageset` marketplace from this repository; the
second installs the `stageset` plugin from it. Restart Claude Code if prompted, and
the skill activates automatically whenever a repository contains StageSet manifests
or the stageset-controller is in play.

## What the skill knows

- **Authoring.** It starts from a minimal StageSet (only `spec.stages` is
  required) and layers on options in order of need — more stages,
  `serviceAccountName` impersonation, `decryption`, per-stage build surface,
  actions, ready checks, conflict policies, update windows, versioned migrations,
  and rollback.
- **Source wiring.** It knows `sourceRef.kind` defaults to `ExternalArtifact`, that
  a stage can point directly at a `GitRepository` / `OCIRepository` / `Bucket`, and
  when to use a producer such as a JaaS `JsonnetSnippet` instead.
- **Operating.** It drives `stagesetctl` (also usable as `kubectl stageset`) to
  preview and reconcile — `diff`, `build`, `get`, `reconcile` — and previews
  changes before applying them.
- **Debugging.** It maps `status.conditions[Ready].reason` to the matching
  [runbook](/runbooks/) and reads `status.stages[]` for per-stage phase, applied
  revision, and executed actions.

The skill treats this site as its source of truth, fetching exact fields and
defaults from the [API reference](/api/stageset/), the [usage](/defining-a-release/) pages, and
the [CLI](/cli/) reference rather than relying on memory.

## Where it lives

The skill source is under `skills/stageset/` — `SKILL.md` plus a
`references/reference.md` cheat-sheet — and the plugin and marketplace manifests
are under `.claude-plugin/`. Edit those files to change what the assistant knows;
the install commands above pull whatever the repository currently ships.
