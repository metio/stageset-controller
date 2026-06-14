# Reason: SourceNotReady

## Symptom

`READY=False`, `REASON=SourceNotReady`. Transient: the controller requeues and clears the condition once the source publishes.

## Cause

A stage's `sourceRef` resolved to an `ExternalArtifact` (directly, or via a producer's RFC-0012 back-pointer such as a JaaS `JsonnetSnippet`), but that artifact's `status.conditions[Ready]` is not yet `True` — its producer has not finished publishing a revision. The StageSet gates on `Ready=True` so it never builds against a half-written artifact.

## Diagnosis

```shell
# Which artifact, and is it Ready?
kubectl get externalartifact -n <namespace>
kubectl describe externalartifact <name> -n <namespace>

# If the producer is a JsonnetSnippet (or other producer kind), check it:
kubectl describe jsonnetsnippet <name> -n <namespace>
```

## Remediation

This usually clears on its own when the producer publishes. If it persists:

- confirm the producing controller (e.g. the JaaS operator, or Flux source-controller) is running and reconciling the producer object;
- check the producer's own Ready condition for an upstream error (a failed render, an unreachable Git/OCI source);
- once the producer reports `Ready=True` with a `status.artifact`, the StageSet converges on the next reconcile.
