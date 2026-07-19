---
title: Image verification
description: Gate a rollout on container image signatures — cluster ImageVerificationPolicies verify each image with cosign keyless identity and pin it to its digest before a stage applies it.
tags: [security, supply-chain, images, cosign, sigstore]
---

Require every container image a rollout deploys to be signed before it reaches the
cluster. You write a cluster-scoped `ImageVerificationPolicy` that names the images it
governs and the identities allowed to sign them; the controller verifies each image a
stage renders and holds the stage if any image fails — nothing unverified is applied.

Verification is owned by the platform, not the tenant: a `StageSet` author deploys
without declaring any signing configuration, and the policies that gate their images
live in one cluster-scoped resource an administrator controls.

## Write a policy

```yaml
apiVersion: stages.metio.wtf/v1
kind: ImageVerificationPolicy
metadata:
  name: acme-images
spec:
  images:
    - "ghcr.io/acme/**"
  authorities:
    - keyless:
        issuer: https://token.actions.githubusercontent.com
        subjectRegExp: ^https://github\.com/acme/.+/\.github/workflows/release\.yml@refs/heads/main$
```

Every image whose repository matches an `images` glob is governed by the policy. A
governed image must carry a Sigstore signature from **at least one** listed authority.
The globs match the ref without its tag or digest: `*` spans one path segment, `**`
spans any number, so `ghcr.io/acme/**` covers `ghcr.io/acme/api` and
`ghcr.io/acme/team/worker` alike.

An authority is a **keyless** identity — the Fulcio certificate a
[cosign](https://docs.sigstore.dev/) keyless signature carries. Match the OIDC issuer
with `issuer` (exact) or `issuerRegExp`, and the certificate subject with `subject`
(exact) or `subjectRegExp`; set exactly one form of each.

## How a stage is gated

Between rendering a stage's objects and applying them, the controller:

1. Extracts every container image from the rendered pod specs (`Pod`, `Deployment`,
   `StatefulSet`, `DaemonSet`, `ReplicaSet`, `Job`, `CronJob`).
2. Selects the policies whose globs match each image.
3. Resolves the image to its digest, fetches the Sigstore bundle cosign attaches as an
   OCI referrer, and verifies the signature against the policy's authorities and the
   resolved digest.
4. Rewrites the image in the manifest to `repository@sha256:…` — the exact digest it
   verified — so a tag cannot be swapped between the check and the apply.

If any governed image fails, the stage is held with `Ready=False` and reason
`ImageUnverified`, and later stages never run. See the
[ImageUnverified runbook](/runbooks/imageunverified/) for the remediation steps.

## Sign images so they verify

Images must carry a **new-format** Sigstore bundle, which cosign attaches to the image
as an OCI referrer:

```shell
cosign sign --new-bundle-format ghcr.io/acme/api@sha256:…
```

An image without a bundle cannot be verified. Mirror and re-sign a third-party image
into your own registry, or exempt it with a `skip` glob:

```yaml
spec:
  images:
    - "ghcr.io/acme/**"
  skip:
    - "ghcr.io/acme/vendored/**"   # audited: a base image that cannot carry a bundle
  authorities:
    - keyless:
        issuer: https://token.actions.githubusercontent.com
        subjectRegExp: ^https://github\.com/acme/.+$
```

A skipped image is recorded, never silently passed.

## Deny images no policy governs

By default an image that matches no policy applies unchanged — verification covers
only what a policy names. Pass `--require-image-verification` to the controller to flip
that to deny-by-default: an image no policy governs holds its stage under
`ImageUnverified`, so a new registry cannot ship unverified because a policy for it was
forgotten.

## Break glass

A genuine emergency — a broken signing pipeline blocking a critical fix — can bypass
the gate for one reconcile by annotating the `StageSet`:

```shell
kubectl annotate stageset acme stages.metio.wtf/skip-image-verification="pipeline outage, INC-1234"
```

The bypass records a `Warning` event naming the reason. It is an operator action; a
tenant editing their own `StageSet` should not hold this power.

## Air-gapped clusters

The controller loads the public Sigstore trusted root over TUF on first use. In a
cluster without egress, point it at an offline trusted-root bundle:

```text
--image-verification-trusted-root=/etc/sigstore/trusted_root.json
```

## Constraints

Authorities are keyless Fulcio certificate identities. A `key` authority or a
`requireAttestations` entry is rejected by the [admission webhook](/security/admission-webhook/)
so a policy never advertises a guarantee the controller does not enforce.
