---
title: TeardownForced
description: A deleting StageSet's finalizer was force-dropped after teardown kept failing past --max-teardown-wait, possibly orphaning objects on the target cluster.
tags: [runbooks, teardown, finalizer, troubleshooting]
---

## Symptom

A StageSet that was deleted lingered in `Terminating`, then disappeared. A `Warning` Event records the force-drop:

```text
kubectl --namespace <ns> get events --field-selector reason=TeardownForced
...
Warning  TeardownForced  TeardownForced after 1h0m0s of failing teardown (delete stage "deploy" objects) —
finalizer dropped; the target cluster may carry orphaned objects an operator must remove by hand. Last error: ...
```

The `stageset_teardown_force_drop_total{namespace,name}` counter increments at the same time.

## Cause

On deletion the controller tears the StageSet's applied objects down in reverse stage order, then drops the finalizer. While a teardown step keeps failing the finalizer is held and the delete retries — so a transient target-cluster outage heals on its own.

But a permanently-unreachable target — a deleted `spec.kubeConfig` Secret, revoked tenant RBAC, or a decommissioned remote cluster — would wedge the StageSet in `Terminating` forever and block namespace teardown. `--max-teardown-wait` (default 1h) caps that wait: once the deletion has been pending longer than the bound, the finalizer is force-dropped so the object can be garbage-collected. Whatever objects the failing stage could not delete are left orphaned.

## Diagnosis

Identify which target and stage failed from the Event message (`Last error` and the operation, e.g. `delete stage "deploy" objects`). Then confirm what was left behind:

```shell
# Objects the controller applies carry owner labels keyed by the StageSet name.
kubectl --context <target-context> get all,configmap,secret --all-namespaces \
    -l stages.metio.wtf/name=<stageset-name>
```

If the target was a remote cluster (`spec.kubeConfig`), check that the kubeconfig Secret and the cloud identity it referenced still exist.

## Remediation

1. Restore access to the target if it is meant to keep running (re-create the kubeConfig Secret, re-grant the tenant SA's RBAC), then delete the orphaned objects with the label selector above.
2. If the target is genuinely gone, the orphaned objects went with it — nothing to clean up.

To make the controller wait longer before force-dropping (for example, to ride out a planned multi-hour target outage), raise `--max-teardown-wait`. Setting it very high re-introduces the original wedge risk, so prefer fixing the target over disabling the escape hatch.
