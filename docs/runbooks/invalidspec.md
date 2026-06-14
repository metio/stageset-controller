# Reason: InvalidSpec

## Symptom

`READY=False`, `REASON=InvalidSpec`. The Message names the offending field or action. Terminal: the controller does not requeue until the spec changes.

## Cause

The spec failed validation that the CRD schema cannot express cheaply, normally one of:

- an **action sets zero or more than one verb** — each action must set exactly one of `patch`, `http`, `wait`, `job` (`delete` is reserved);
- a **reserved post-v1 field** is populated — `spec.version`, `spec.migrations`, `spec.rollbackOnFailure`, `stage.conflictPolicy`, or the `delete` action verb. These have stable shapes but are not implemented yet, so they are rejected rather than silently ignored;
- **two stages claim the same object** in one run (an ambiguous inventory).

The admission webhook normally rejects these at write time; seeing this on the object means the webhook was bypassed or disabled and the reconciler caught it.

## Diagnosis

```shell
kubectl describe stageset <name> -n <namespace>
```

Read the Message — it names the stage, action, or field.

## Remediation

Fix the spec per the Message:

- give each action exactly one verb;
- remove any reserved field (see the design doc's "reserved post-v1 API");
- ensure no object is rendered by two stages — move it to a single stage, or split it.

If the webhook should have caught this, confirm the `ValidatingWebhookConfiguration` is installed and its service is reachable.
