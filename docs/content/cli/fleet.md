---
title: stagesetctl fleet
description: Show a FleetRollout's wave-by-wave progress — which members are at the target, held, or regressed.
tags: [cli, fleet, operations]
---

`stagesetctl fleet NAME` shows where a [`FleetRollout`](/gating/fleet-rollout/) is,
the way [`plan`](/cli/plan/) shows a StageSet: its overall phase and target version,
each wave's progress, and — per member — whether it has reached the version, is still
held awaiting its wave, or has regressed. Read-only.

```text
stagesetctl fleet NAME [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--output`, `-o` | _(human view)_ | `yaml` or `json` to emit the FleetRollout object. |

## Example

```text
$ stagesetctl fleet moodle-2-0
FleetRollout moodle-2-0  →  version 2.0.0
  phase: InProgress   wave: broad
  wave canary   2/2 at 2.0.0, 2 ready   settled, health: Passing
    ✓ moodle-acme/app             2.0.0  Ready
    ✓ moodle-beta/app             2.0.0  Ready
  wave broad    1/3 at 2.0.0, 3 ready
    ✓ moodle-x/app                2.0.0  Ready
    … moodle-y/app                1.0.0  held → awaiting approval
    … moodle-z/app                1.0.0  held → awaiting approval
  wave rest
    … moodle-old/app              1.0.0  held → awaiting approval
```

Each member carries a mark: **✓** at the target version and Ready, **⚠** at the
target but not Ready (regressed), **…** still on the old version and held awaiting its
wave. When the rollout is halted, the halt reason and message print under the phase.

Membership is resolved the same way the controller resolves it (`selector` bounded by
`namespaceSelector`), so the view matches what the rollout acts on.
