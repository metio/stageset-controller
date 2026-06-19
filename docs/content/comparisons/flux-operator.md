---
title: vs Flux Operator ResourceSet
description: Horizontal fan-out and scheduled discovery versus vertical, gated promotion.
tags: [comparison, flux-operator, resourceset, stages]
---

`StageSet` and the [Flux Operator](https://fluxoperator.dev/)'s `ResourceSet`
solve different axes of delivery, and they compose cleanly:

- **`ResourceSet` is horizontal fan-out.** It templates *N* instances of a set of
  resources from a matrix of inputs, and pairs with a
  `ResourceSetInputProvider` (RSIP) to *discover* those inputs — pull requests,
  branches, tags, OCI artifact tags — on a schedule.
- **`StageSet` is vertical promotion.** It moves *one* application through an
  ordered list of gated [stages](/usage/stages-and-sources/), with typed
  [actions](/usage/actions/), [versioned migrations](/usage/versioned-migrations/),
  and [rollback](/usage/rollback/) along the way.

Reach for `ResourceSet` to stamp out many similar deployments from a changing set
of inputs. Reach for `StageSet` to roll a single release out in order, gated at
each step. To do both — discover a new version, then promote it through gated
stages — run them together; see the
[Flux Operator integration](/usage/flux-operator/) page.

## What ResourceSet gives you

`ResourceSet` (`fluxcd.controlplane.io/v1`) renders Kubernetes resources from a
template, fanned out over inputs:

- **Inputs.** `spec.inputs` holds static key-value sets; `spec.inputsFrom`
  references one or more `ResourceSetInputProvider` objects that supply inputs
  dynamically. `spec.inputStrategy` combines them by `Flatten` or `Permute`.
- **Templating.** `spec.resources` (or `spec.resourcesTemplate` for one YAML
  string) carries Go templates with `<< >>` delimiters, e.g. `<< inputs.tag >>`,
  plus sprig-style helpers.
- **Discovery.** A `ResourceSetInputProvider` (`spec.type` of `Static`,
  `GitHubPullRequest`, `OCIArtifactTag`, and others) exports inputs to
  `status.exportedInputs`, optionally filtered by `spec.filter` (semver, regex,
  limit). Its `spec.schedule` (cron + `timeZone` + `window`) controls *when* it
  re-checks the source for new inputs — the Flux Operator's
  [time-based delivery](https://fluxoperator.dev/docs/resourcesets/time-based-delivery/).
- **Apply.** `spec.dependsOn`, `spec.wait`, and `spec.serviceAccountName` govern
  ordering between objects, health waiting, and impersonation.

What `ResourceSet` does not model is an ordered, gated rollout of a single app:
there are no per-stage gates, no typed migrations crossing a version boundary, and
no automatic rollback to a previous revision.

## What StageSet adds

- **Ordered, gated stages.** `spec.stages` runs top to bottom; each waits for the
  previous to report healthy via [ready checks](/usage/ready-checks/) before the
  next begins.
- **Typed actions between steps.** `patch`, `http`, `wait`, `job`, `delete`, and
  `apply` [actions](/usage/actions/) run as `pre` / `post` / `onFailure` gates
  around a stage.
- **Version-gated migrations.** [`spec.migrations`](/usage/versioned-migrations/)
  run an action ladder exactly once when crossing a version boundary.
- **Rollback.** [`rollbackOnFailure`](/usage/rollback/) restores the last good
  revisions when a run fails.

## Both time-gate delivery

Both tools can gate delivery on a clock, but at different layers:

| Layer | Field | Gates |
|---|---|---|
| `ResourceSetInputProvider` | `spec.schedule` (cron, `timeZone`, `window`) | *when inputs refresh* — how often a new version or PR is discovered |
| `StageSet` | [`spec.updateWindows`](/usage/update-windows/) (`Allow` / `Deny`) | *when a discovered revision rolls out* through the stages |

When you combine them, gate at **one** layer to avoid double-gating. Either let the
RSIP `schedule` constrain when a new version becomes visible and leave the
`StageSet` always-open, or refresh inputs freely and let the `StageSet`
`updateWindows` decide when the promotion happens. The
[integration page](/usage/flux-operator/) shows both.

## Using them together

The two compose along their separate axes: a `ResourceSetInputProvider` discovers
a new version on a schedule, a `ResourceSet` templates a `StageSet` pinned to that
version, and the `StageSet` runs the ordered, gated promotion. The other direction
fans a `ResourceSet` out into one `StageSet` per tenant, branch, or pull request.
Both patterns, with complete manifests, are on the
[Flux Operator integration](/usage/flux-operator/) page.
