---
title: ArtifactNotFound
description: The referenced ExternalArtifact could not be found (transient; the controller requeues).
tags: [runbooks, externalartifact, sources, troubleshooting]
---

## Symptom

`READY=False`, `REASON=ArtifactNotFound`. Transient: the controller requeues in case the artifact appears.

## Cause

A stage's `sourceRef` resolves to **no `ExternalArtifact`**. Either:

- a **direct** `sourceRef` (`kind: ExternalArtifact`, the default) names an object that does not exist in the target namespace; or
- a **producer** `sourceRef` (e.g. `kind: JsonnetSnippet`) exists, but no `ExternalArtifact` carries a `spec.sourceRef` back-pointer to it yet — the producer has not created its artifact object.

## Diagnosis

```shell
kubectl --namespace <namespace> describe stageset <name>     # Message names the missing ref
kubectl --namespace <namespace> get externalartifact
```

For a producer ref, confirm the producer object exists and that it is configured to publish an `ExternalArtifact` (not only serve over HTTP):

```shell
kubectl --namespace <namespace> get <producer-kind> <name> --output yaml
```

## Remediation

- Fix a typo in `sourceRef.name` / `sourceRef.namespace`.
- For a direct ref, create (or wait for) the named `ExternalArtifact`.
- For a producer ref, ensure the producer actually publishes an artifact and that it lands in the same namespace as the StageSet (cross-namespace producer refs are gated by `--no-cross-namespace-refs`).

If the artifact exists but is not yet published, the reason is [`SourceNotReady`](/runbooks/sourcenotready/); a spec/API resolution failure is [`ResolveFailed`](/runbooks/resolvefailed/). See [stages and sources](/usage/stages-and-sources/).
