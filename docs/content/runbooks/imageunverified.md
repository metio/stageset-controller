---
title: ImageUnverified
description: A stage referenced an image that fails an ImageVerificationPolicy; the stage is held before apply.
tags: [runbooks, security, supply-chain, images, troubleshooting]
---

## Symptom

`READY=False`, `REASON=ImageUnverified`. The run halts at the stage before it applies
anything. The Message names the failing image and the policy that rejected it.

## Cause

Before a stage applies its objects, the controller verifies every container image they
reference against the cluster [`ImageVerificationPolicy`](/security/image-verification/)
resources — signature (cosign keyless identity or public key) and any required
attestations (SLSA provenance, SBOM, a fresh scan). The stage is held because an image:

- is not signed, or is signed by an identity no policy authority accepts;
- is missing a required attestation, or one is stale (past `maxAge`); or
- (under `--require-image-verification`) matches no policy at all — deny-by-default.

Nothing unverified is deployed — the gate runs *before* apply.

## Diagnosis

```shell
kubectl --namespace <namespace> describe stageset <name>   # the message names the image + policy
kubectl get imageverificationpolicies                      # the governing policies
```

Verify the image out of band with the same identity the policy expects:

```shell
cosign verify --certificate-identity-regexp <subject> --certificate-oidc-issuer <issuer> <image>
```

## Remediation

- **The image should be trusted but isn't signed/attested:** fix the build pipeline to
  sign it (`cosign sign --new-bundle-format`) and attach the required attestations,
  then re-publish. The stage clears on the next reconcile.
- **A third-party image can't carry a bundle:** mirror and re-sign it into your own
  registry, or add its ref to a policy's `skip` list (an audited exemption).
- **The policy is wrong** (an identity/attestation it shouldn't require): correct the
  `ImageVerificationPolicy`.
- **A genuine emergency:** the break-glass annotation
  `stages.metio.wtf/skip-image-verification=<reason>` bypasses the gate for one
  reconcile and records a Warning event — an operator action, never a tenant's.
