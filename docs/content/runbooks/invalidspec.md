---
title: InvalidSpec
description: The StageSet spec is invalid; the Message names the offending field or action.
tags: [runbooks, troubleshooting, api]
---

## Symptom

`READY=False`, `REASON=InvalidSpec`. The Message names the offending field or action. Terminal: the controller does not requeue until the spec changes.

## Cause

The spec failed validation that the CRD schema cannot express cheaply, normally one of:

- an **action sets zero or more than one verb** — each action must set exactly one of `patch`, `http`, `wait`, `job`, `delete`, `apply` (see [actions](/usage/actions/));
- **`spec.migrations` without `spec.version`**, or a migration anchored to a stage name that does not exist (see [versioned migrations](/usage/versioned-migrations/));
- **`spec.version` does not name exactly one source** — set one of `value`, `fromObject`, or `fromArtifact`;
- **`spec.decryption.provider` is not `sops`**, or a `secretRef` is given without a `name` (see [encryption](/usage/encryption/));
- an **invalid update window** — a malformed `schedule`, `duration`, or `timeZone` (see [update windows](/usage/update-windows/)).

The admission webhook normally rejects these at write time; seeing this on the object means the webhook was bypassed or disabled and the reconciler caught it.

## Diagnosis

```shell
kubectl --namespace <namespace> describe stageset <name>
```

Read the Message — it names the stage, action, or field.

## Remediation

Fix the spec per the Message:

- give each action exactly one verb;
- set `spec.version` (to one of `value`/`fromObject`/`fromArtifact`) whenever `spec.migrations` is present, and anchor each migration to a real stage;
- set exactly one `spec.version` source;
- use `provider: sops` for `spec.decryption`, with a named `secretRef` when one is given;
- correct any malformed update window (`schedule`, `duration`, `timeZone`).

If the webhook should have caught this, confirm the `ValidatingWebhookConfiguration` is installed and its service is reachable.
