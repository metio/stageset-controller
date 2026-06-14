# Reason: ArtifactNotFound

## Symptom

`READY=False`, `REASON=ArtifactNotFound`. Transient: the controller requeues in case the artifact appears.

## Cause

A stage's `sourceRef` resolves to **no `ExternalArtifact`**. Either:

- a **direct** `sourceRef` (`kind: ExternalArtifact`, the default) names an object that does not exist in the target namespace; or
- a **producer** `sourceRef` (e.g. `kind: JsonnetSnippet`) exists, but no `ExternalArtifact` carries a `spec.sourceRef` back-pointer to it yet — the producer has not created its artifact object.

## Diagnosis

```shell
kubectl describe stageset <name> -n <namespace>     # Message names the missing ref
kubectl get externalartifact -n <namespace>
```

For a producer ref, confirm the producer object exists and that it is configured to publish an `ExternalArtifact` (not only serve over HTTP):

```shell
kubectl get <producer-kind> <name> -n <namespace> -o yaml
```

## Remediation

- Fix a typo in `sourceRef.name` / `sourceRef.namespace`.
- For a direct ref, create (or wait for) the named `ExternalArtifact`.
- For a producer ref, ensure the producer actually publishes an artifact and that it lands in the same namespace as the StageSet (cross-namespace producer refs are gated by `--no-cross-namespace-refs`).
