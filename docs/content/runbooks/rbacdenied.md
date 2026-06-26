---
title: RBACDenied
description: An apiserver call the controller made — resolving a source, an impersonated apply, or a tenant get — failed with Forbidden or referenced a kind the apiserver does not know.
tags: [runbooks, troubleshooting, rbac]
---

## Symptom

`READY=False`, `REASON=RBACDenied`. The Message names the call that failed and appends the verbatim apiserver error:

```text
kubectl --namespace <ns> describe stageset <name>
...
Status:
  Conditions:
    Reason:  RBACDenied
    Status:  False
    Type:    Ready
    Message: stage "deploy" apply: RBAC denied apply — grant the missing verb to the tenant ServiceAccount ...
```

The controller logs at error level and stops engaging backoff for this StageSet. The next reconcile happens only when the spec changes, a referenced source CR's status flips, or `spec.interval` ticks — so the workqueue isn't burning cycles on a permanently-failing call.

## Cause

The apiserver returned `Forbidden`, reported the resource kind is not registered (the CRD is not installed), or rejected the payload as schema-invalid. Three call sites surface this:

1. **Source resolution.** The controller (or the impersonated `spec.serviceAccountName`) lacks `get` / `list` on the source kind named by a stage's `sourceRef`, or the source-controller CRDs are not installed.
2. **Connect to target cluster.** Minting the tenant SA's TokenRequest token, or building the remote-cluster connection, was denied.
3. **Apply.** The impersonated tenant SA lacks `create` / `update` / `patch` on a resource the stage applies, or an admission webhook rejected the manifest.

## Diagnosis

`kubectl describe` shows the classified message with the verbatim apiserver error appended, so you can read off the SA, the verb it lacked, and the resource. Verify the SA's permissions:

```shell
kubectl --namespace <tenant-namespace> get sa <sa-name>
kubectl auth can-i --as=system:serviceaccount:<tenant-namespace>:<sa-name> \
    --namespace <tenant-namespace> \
    <verb> <resource>
```

For the missing-CRD variant:

```shell
kubectl get crd | grep -E 'source.toolkit.fluxcd.io'
# If source-controller's CRDs are missing, install Flux:
# https://fluxcd.io/flux/get-started/
```

## Remediation

Grant the missing verb to the controller's ClusterRole or to the tenant `ServiceAccount`'s `Role`. The expected verbs are documented in the [Tenancy and RBAC](/security/multi-cluster/) guide. After the RBAC change (or after installing the missing CRD), force the next reconcile — the non-transient classification means the last spec edit does not auto-retrigger:

```shell
kubectl --namespace <ns> annotate stageset <name> reconcile.fluxcd.io/requestedAt=$(date -u +%FT%TZ) --overwrite
```

## Why this is non-transient

`Forbidden` does not recover by retry — the cluster operator has to grant the verb. A missing CRD does not appear by retrying. A schema-invalid payload is rejected the same way every time. Treating these as terminal keeps the workqueue-depth metric meaningful: anything on it is genuinely live work, not a permanently-failing call piling up wasted API requests every interval.
