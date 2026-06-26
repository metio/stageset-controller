---
title: ResolveFailed
description: A source reference could not be resolved to a ready ExternalArtifact.
tags: [runbooks, sources, externalartifact, troubleshooting]
---

## Symptom

`READY=False`, `REASON=ResolveFailed`. The Message describes why resolution failed.

## Cause

A stage's `sourceRef` could not be resolved to an `ExternalArtifact` for a spec/config or API reason (distinct from "not published yet", which is [`SourceNotReady`](/runbooks/sourcenotready/), and "no such object", which is [`ArtifactNotFound`](/runbooks/artifactnotfound/)). Common cases:

- an **ambiguous producer** — more than one `ExternalArtifact` back-points at the same producer object, so the target is undefined;
- a **cross-namespace ref rejected** by `--no-cross-namespace-refs`;
- a **transient API error** reading the source or artifact (a list/get blip). A permanent API error — RBAC denial or a missing source CRD — is reported as [`RBACDenied`](/runbooks/rbacdenied/) instead, since retry can't fix it.

When the failing `sourceRef` targets another namespace, the Message is deliberately scrubbed to `cross-namespace <kind> %q is not reachable` so tenants cannot fingerprint other namespaces — check that source CR's status in its own namespace.

## Diagnosis

```shell
kubectl --namespace <namespace> describe stageset <name>
# Ambiguity: are there multiple artifacts pointing at the producer?
kubectl --namespace <namespace> get externalartifact --output yaml | grep -A3 sourceRef
```

## Remediation

- **Ambiguous producer:** ensure exactly one `ExternalArtifact` back-points at the producer, or reference the `ExternalArtifact` directly by name.
- **Cross-namespace rejected:** move the source into the StageSet's namespace, or run the controller without `--no-cross-namespace-refs` if your [tenancy model](/security/multi-cluster/) allows it.
- **RBAC / missing CRD:** these surface as [`RBACDenied`](/runbooks/rbacdenied/) — grant the controller (or the impersonated `serviceAccountName`) read on the source kind, or install the `source-controller` CRDs.
