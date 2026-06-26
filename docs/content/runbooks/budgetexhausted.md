---
title: BudgetExhausted
description: A rollout is frozen because the service is out of its SLO error budget.
tags: [runbooks, error-budget, slo, scheduling, troubleshooting]
---

## Symptom

`READY=False`, `REASON=BudgetExhausted` (or, for an already-deployed StageSet, `READY=True` with a `status.budgetFreeze` and the message prefixed `Deployed;`). `status.budgetFreeze` records the last observed remaining budget, the freeze and resume thresholds, and when the freeze began.

## Cause

This is **not a failure** — it is the [error-budget freeze](/gating/error-budget/) working as configured. The metric source named in `spec.errorBudget.source` returned a remaining-budget value below `freezeThreshold`, so new-revision rollouts are held until the budget recovers. This is the Google SRE error-budget policy: when the budget is spent, stop shipping feature changes until reliability recovers.

The freeze holds only new-revision rollouts. Drift on the current revision keeps being corrected — a frozen service still has its declared state enforced — and the freeze self-resumes once the remaining budget reaches `resumeThreshold`.

## Diagnosis

```shell
kubectl --namespace <namespace> get stageset <name> --output jsonpath='{.status.budgetFreeze}'
kubectl --namespace <namespace> get stageset <name> --output jsonpath='{.spec.errorBudget}'
```

Compare `status.budgetFreeze.remaining` against `freezeThreshold` / `resumeThreshold`. Confirm the source query returns what you expect — a value that never matches reality points at a wrong query (the `stageset_budget_remaining` metric carries the same number). A *calendar* budget source snaps back to full at the period boundary and unfreezes instantly; a *rolling* source recovers gradually.

## Remediation

Usually none — the rollout resumes on its own once the budget recovers. To ship a reliability or security fix while still out of budget (the policy's explicit exemption), break the glass once:

```shell
stagesetctl reconcile <name> --namespace <namespace> --budget-override
```

That applies the held rollout a single time without disabling the gate. To change the policy, adjust `freezeThreshold` / `resumeThreshold`, or set `spec.errorBudget.dryRun: true` to observe what *would* freeze without holding anything while you tune the query.
