---
title: Runbooks
description: Symptom-to-remediation pages for every StageSet status reason and operational alert, grouped by what each means for the rollout.
tags: [runbooks, troubleshooting, operations]
---

Start from the `status.conditions[Ready].reason` on a StageSet, or from a firing
operational alert, and follow the matching page to diagnose and remediate the
symptom.

The controller appends the matching page link to each actionable Ready message —
`(runbook: https://stageset.projects.metio.wtf/runbooks/<reason>/)` — so a
`kubectl describe` routes you straight here. Healthy and intentional reasons
carry no link.

The reasons below are grouped by what they mean for the rollout. If you are
paging in a hurry, check **Healthy and intentional holds** first — those are not
failures.

## Healthy and intentional holds

The rollout is doing exactly what it was told to. No action is needed beyond the
one the gate is waiting for — approve, promote, resume, or wait for the window.

| Reason | What it means |
|---|---|
| [`Succeeded`](/runbooks/succeeded/) | All stages applied and verified; healthy steady state. |
| [`Suspended`](/runbooks/suspended/) | Reconciliation is paused via `spec.suspend`. |
| [`UpdateDeferred`](/runbooks/updatedeferred/) | A new revision is held by a closed update window. |
| [`Soaking`](/runbooks/soaking/) | A healthy stage is holding through its soak window before advancing. |
| [`AwaitingPromotion`](/runbooks/awaitingpromotion/) | A healthy stage is holding for a manual promotion. |
| [`AwaitingApproval`](/runbooks/awaitingapproval/) | A version transition is held for operator (or FleetRollout) approval. |
| [`BudgetExhausted`](/runbooks/budgetexhausted/) | A rollout is frozen because the service is out of its SLO error budget; it resumes on its own when the budget recovers. |

## Waiting on upstream

Blocked on something outside the StageSet. These usually clear on their own once
the upstream becomes ready; check the referenced source or dependency.

| Reason | What it means |
|---|---|
| [`SourceNotReady`](/runbooks/sourcenotready/) | The source exists but has not published a ready artifact yet. |
| [`ArtifactNotFound`](/runbooks/artifactnotfound/) | The referenced `ExternalArtifact` could not be found; the controller requeues. |
| [`ResolveFailed`](/runbooks/resolvefailed/) | A source reference could not be resolved to a ready `ExternalArtifact`. |
| [`DependencyNotReady`](/runbooks/dependencynotready/) | A StageSet named in `spec.dependsOn` is not yet Ready. |

## Failures that need action

Something is wrong with the StageSet, its sources, or its permissions, and the
rollout will not progress until you fix it.

| Reason | What it means |
|---|---|
| [`StageFailed`](/runbooks/stagefailed/) | A stage failed to fetch, build, apply, verify, or run an action. |
| [`Stalled`](/runbooks/stalled/) | The run cannot make progress and will not retry until the spec changes. |
| [`InvalidSpec`](/runbooks/invalidspec/) | The spec is invalid; the Message names the offending field or action. |
| [`InvalidVersion`](/runbooks/invalidversion/) | A version source or value could not be parsed as semver. |
| [`RBACDenied`](/runbooks/rbacdenied/) | An apiserver call failed with Forbidden, or referenced a kind the apiserver does not know. |
| [`PromotionBlocked`](/runbooks/promotionblocked/) | A stage's promotion analysis breached its thresholds. |
| [`PreviousRevisionUnavailable`](/runbooks/previousrevisionunavailable/) | `rollbackOnFailure` is set but the last-good revisions could not be restored. |
| [`BudgetSourceUnavailable`](/runbooks/budgetsourceunavailable/) | A metric source for an error-budget freeze or a promotion analysis could not be read. |
| [`TeardownForced`](/runbooks/teardown-forced/) | A deleting StageSet's finalizer was force-dropped after teardown kept failing, possibly orphaning objects. |

## Migration problems

A version-aware migration is holding the rollout back. The version stays pinned
to a safe revision until the migration is fixed or approved.

| Reason | What it means |
|---|---|
| [`MigrationFailed`](/runbooks/migrationfailed/) | A migration's action failed; the run halts and retries with backoff. |
| [`MigrationDirty`](/runbooks/migrationdirty/) | A migration failed repeatedly; auto-retry is halted until a manual reconcile clears it. |
| [`MigrationArtifactInvalid`](/runbooks/migrationartifactinvalid/) | A sourced migration ladder could not be parsed or failed content validation. |
| [`MigrationCoverageMissing`](/runbooks/migrationcoveragemissing/) | A major-version transition has no covering migration and coverage is required. |
| [`MigrationStageNotFound`](/runbooks/migrationstagenotfound/) | A migration anchors to a stage that does not exist in this StageSet. |
| [`MigrationSourceNotPinned`](/runbooks/migrationsourcenotpinned/) | A sourced ladder is pinned to a mutable tag or branch instead of an immutable digest or commit. |
| [`MigrationSourceNotVerified`](/runbooks/migrationsourcenotverified/) | A sourced ladder is not signature-verified; the destructive ladder is refused. |
| [`DowngradeRequiresMigration`](/runbooks/downgraderequiresmigration/) | The desired version is below the deployed version across a migration boundary. |

## Operational alerts

These are reached from a firing Prometheus alert rather than a Ready reason —
they describe the controller's own health, not a single StageSet.

| Alert | Page | What it means |
|---|---|---|
| `StageSetControllerPodDown` | [Controller pod down](/runbooks/controller-pod-down/) | A controller pod has been NotReady for the alert window. |
| `StageSetReconcileLatencyHigh` | [Reconcile latency high](/runbooks/reconcile-latency/) | Reconcile p99 latency is above threshold. |
| `StageSetControllerWorkqueueDepthHigh` | [Workqueue saturation](/runbooks/workqueue-saturation/) | The controller cannot drain its reconcile queue fast enough. |
| `StageSetWebhookCertRenewalFailing` | [Webhook cert renewal failing](/runbooks/webhook-cert-renewal/) | The self-signed admission webhook certificate is not being rotated. |
| `StageSetWatchEngagementFailing` | [Producer watch engagement failing](/runbooks/watch-engagement/) | A dynamic watch on a producer source kind failed to engage, so referencing StageSets stop re-triggering on its changes. |
