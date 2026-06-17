---
title: SourceNotReady
description: The source exists but has not published a ready artifact yet (transient).
tags: [runbooks, sources, troubleshooting]
---

## Symptom

`READY=False`, `REASON=SourceNotReady`. Transient: the controller requeues and clears the condition once the source publishes.

## Cause

A stage's `sourceRef` resolved to an `ExternalArtifact` (directly, or via a producer's RFC-0012 back-pointer such as a JaaS `JsonnetSnippet`), but that artifact's `status.conditions[Ready]` is not yet `True` — its producer has not finished publishing a revision. The StageSet gates on `Ready=True` so it never builds against a half-written artifact.

## Diagnosis

```shell
# Which artifact, and is it Ready?
kubectl --namespace <namespace> get externalartifact
kubectl --namespace <namespace> describe externalartifact <name>

# If the producer is a JsonnetSnippet (or other producer kind), check it:
kubectl --namespace <namespace> describe jsonnetsnippet <name>
```

## Remediation

This usually clears on its own when the producer publishes. If it persists:

- confirm the producing controller (e.g. the JaaS operator, or [Flux](https://fluxcd.io/) `source-controller`) is running and reconciling the producer object;
- check the producer's own Ready condition for an upstream error (a failed render, an unreachable `GitRepository`/`OCIRepository` source);
- once the producer reports `Ready=True` with a `status.artifact`, the StageSet converges on the next reconcile.

If the artifact never appears at all, the reason is [`ArtifactNotFound`](/runbooks/artifactnotfound/); a spec/API resolution failure is [`ResolveFailed`](/runbooks/resolvefailed/). See [stages and sources](/usage/stages-and-sources/).
