---
title: Admission webhook
description: The validating webhook for StageSet — the invariants it enforces, why it exists instead of CEL, the failure-policy trade-off, and the two TLS provisioning modes.
tags: [webhook, admission, security, tls]
---

A StageSet's spec carries invariants the CRD's structural schema cannot enforce
cheaply, so the controller ships a **validating admission webhook** that rejects a
bad StageSet at `kubectl apply` time — on `create` and `update` of
`stages.metio.wtf/v1` StageSets — before it ever reaches a reconcile.

## Why a webhook and not CEL

The natural home for these rules is a CRD CEL validation. The catch: CEL cost is
multiplied by the size of every enclosing array, and a StageSet's `spec.stages` and
each stage's action lists are deliberately **unbounded**. A CEL rule over them
makes the apiserver reject the CRD on cost grounds. Moving the checks into a webhook
keeps the lists unbounded while still validating them.

## What it validates

A single `ValidateSpec` routine is the source of truth. It enforces:

- **One operation per action** — every entry in `actions.pre`/`post`/`onFailure`
  (and a migration's actions) sets exactly one of `patch` / `http` / `wait` /
  `job` / `delete` / `apply`. Zero or two is rejected. See [actions](/usage/actions/).
- **One version source** — when `spec.version` is set, exactly one of `value`,
  `fromObject`, or `fromArtifact` (see [versioned migrations](/usage/versioned-migrations/)).
- **Coherent migrations** — `spec.migrations` require a `spec.version`, each
  anchors to a real stage, and each migration action sets exactly one verb.
- **Decryption shape** — `spec.decryption`, when set, names the only supported
  provider (`sops`), and a given `secretRef` carries a `name` (see
  [secrets encryption](/usage/encryption/)).
- Reserved post-v1 fields are rejected up front.

The same `ValidateSpec` also runs **inside the reconciler as a fallback**, so a
StageSet that slips past a disabled or bypassed webhook still fails loudly — it just
fails at reconcile (`Ready=False`) instead of at admission.

## Failure-policy trade-off

The webhook is registered with `failurePolicy: Fail`: if its endpoint is
unreachable, the apiserver **rejects** StageSet create/update rather than admit an
unvalidated object. During a rolling update that window is brief — the next replica
serves admission. If your GitOps tooling cannot tolerate even that, the
reconciler-side fallback lets you run with the webhook **disabled**
(`webhook.enabled: false`) and still catch invalid specs; they fail at reconcile
instead of at admission.

## TLS provisioning

The apiserver dials the webhook over TLS and verifies the serving certificate
against the `caBundle` on the `ValidatingWebhookConfiguration`. Choose how that is
provisioned with `--webhook-cert-mode` (chart value `webhook.certMode`):

### cert-manager (default)

The chart renders a `cert-manager.io/v1` Certificate; cert-manager issues the
Secret, it is mounted at `--webhook-cert-dir`, and cert-manager renews it. The
webhook server hot-reloads the rotated files. Requires
[cert-manager](https://cert-manager.io/) in the cluster.

### self-signed

No cert-manager dependency: the controller generates an ECDSA CA and serving
certificate in-pod, writes them to `--webhook-cert-dir`, and patches the named
`ValidatingWebhookConfiguration`'s `caBundle` so the apiserver trusts the chain
(`--webhook-validating-config-name` is required). The `caBundle` is treated as a CA
**set** — each replica unions its own CA in and prunes only its own expired blocks,
so several replicas converge during a rolling update instead of clobbering each
other's trust. A renewer goroutine regenerates at `validity/3`
(`--webhook-cert-validity`, default one year) and the server hot-reloads without a
restart.

```yaml
# Helm values
webhook:
  enabled: true
  certMode: cert-manager     # or self-signed
```

The [configuration reference](/installation/configuration/) lists the full
`--webhook-*` flag set.
