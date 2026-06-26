---
title: Security & multi-tenancy
description: Run the controller safely — SOPS decryption, tenant impersonation, remote clusters, admission validation, network policy, and service mesh.
tags: [security, multi-tenancy, rbac]
---

How to run StageSet without granting it broad cluster power, keep secrets
encrypted, and constrain what it can reach. The controller applies as a tenant
`ServiceAccount`, so a StageSet can do exactly what its RBAC allows and no more.

- **[Secrets encryption](/security/encryption/)** — decrypt SOPS-encrypted files
  in memory during apply.
- **[Multi-cluster and tenancy](/security/multi-cluster/)** — apply as a tenant
  `ServiceAccount` or to a remote cluster.
- **[Admission webhook](/security/admission-webhook/)** — validate StageSets at
  admission time.
- **[Network policy](/security/network-policy/)** — lock the controller's traffic
  down to what it needs.
- **[Service mesh](/security/service-mesh/)** — run the controller inside a mesh.
</content>
