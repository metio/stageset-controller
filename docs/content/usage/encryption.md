---
title: Secrets encryption (SOPS)
description: Decrypt SOPS-encrypted files in a stage's source before they are applied.
tags: [secrets, encryption, sops, security]
---

A stage's source can carry [SOPS](https://github.com/getsops/sops)-encrypted
files — typically a `Secret` whose values are encrypted — and the controller
decrypts them in memory, before building and applying the manifests. This mirrors
Flux's `kustomize-controller` decryption contract, so an existing SOPS-encrypted
repository works unchanged.

Set `spec.decryption` and point it at a Secret holding the keys:

```yaml
apiVersion: stages.metio.wtf/v1
kind: StageSet
metadata:
  name: payments
  namespace: payments
spec:
  serviceAccountName: payments-deployer
  decryption:
    provider: sops          # the only provider
    secretRef:
      name: sops-age        # a Secret in this namespace holding the age key
  stages:
    - name: app
      sourceRef:
        kind: GitRepository
        name: payments-config   # contains an encrypted secret.yaml
```

## Walkthrough — age

[age](https://age-encryption.org/) is the simplest key type and needs no external
service. Take a `Secret` from plaintext to a GitOps-safe rollout in four steps.

**1. Generate an age key.** The file holds the private key; the printed `age1…`
line is the public recipient to encrypt to.

```bash
age-keygen -o age.agekey
# public key: age1qz…
```

**2. Encrypt a Secret.** Encrypt only its values, so the file stays a valid
Kubernetes object, then commit `secret.enc.yaml` (never the plaintext):

```yaml
# secret.yaml
apiVersion: v1
kind: Secret
metadata:
  name: payments-db
  namespace: payments
stringData:
  password: s3cr3t-do-not-commit-plaintext
```

```bash
sops --encrypt --age age1qz… \
  --encrypted-regex '^(data|stringData)$' \
  secret.yaml > secret.enc.yaml
```

**3. Put the private key in the cluster** under a `.agekey` data entry. Store
`age.agekey` itself somewhere safe — it is the only thing that can decrypt the
Secret.

```bash
kubectl create secret generic sops-age \
  --namespace payments \
  --from-file=keys.agekey=age.agekey
```

**4. Decrypt on rollout.** Point a `StageSet` at the source holding
`secret.enc.yaml` and set `spec.decryption` (as in the example above). On reconcile
the controller fetches the source, decrypts every SOPS file in memory, builds, and
applies — so the cluster holds the plaintext `payments-db` Secret while Git only
ever held ciphertext. Grant the deployer ServiceAccount read access to the key
Secret (see [tenancy](#how-keys-are-read--tenancy) below).

## Pairing with JaaS-rendered manifests

A realistic app renders its config from Jsonnet with
[JaaS](https://jaas.projects.metio.wtf/) and keeps only its Secret encrypted. The
two compose cleanly because each owns one concern:

- **JaaS renders the non-secret manifests.** It evaluates Jsonnet server-side and
  cannot hold secret values: SOPS ciphertext carries a MAC over the whole encrypted
  document, so it can't be authored in Jsonnet — and routing plaintext secrets
  through a render service is what you are avoiding.
- **The Secret stays SOPS-encrypted in Git**, as in the walkthrough.
- **The controller decrypts and orders both** under one `spec.decryption`:

```yaml
spec:
  serviceAccountName: payments-deployer
  decryption:
    provider: sops
    secretRef:
      name: sops-age
  stages:
    - name: secrets                 # decrypt + apply the SOPS Secret first
      sourceRef:
        kind: GitRepository
        name: payments-secrets
    - name: app                     # then the JaaS-rendered app that mounts it
      sourceRef:
        apiVersion: jaas.metio.wtf/v1
        kind: JsonnetSnippet
        name: payments-app
```

The `secrets` stage runs first; only once the `Secret` is applied does the `app`
stage roll out the rendered manifests that mount it. The encrypted Secret and the
rendered config live in separate sources, so the Jsonnet author never touches secret
material.

## The fields

- **`provider`** — the decryption backend. Only `sops` is supported.
- **`secretRef.name`** — a Secret in the `StageSet`'s namespace holding the keys,
  using the SOPS conventions: age private keys under data entries ending in
  `.agekey`, armored PGP private keys under `.asc`. Optional — omit it for a
  [cloud-KMS-only](#cloud-kms) setup.

## How keys are read — tenancy

The key Secret is read in the `StageSet`'s namespace **under its
`serviceAccountName`**, exactly like the manifests it applies. A tenant can only
decrypt with key material its own ServiceAccount is allowed to read, so a key in one
namespace is never reachable from another. Grant the deployer SA `get` on the key
Secret:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: payments-deployer-sops
  namespace: payments
rules:
  - apiGroups: [""]
    resources: [secrets]
    resourceNames: [sops-age]
    verbs: [get]
```

In a [single-tenant cluster-admin](/usage/multi-cluster/#single-tenant-cluster-admin)
install (no `serviceAccountName`), the controller reads the key Secret under its
own identity instead.

## Decryption and the rollback store

Decrypted bytes exist only in memory on the apply path. The one place rendered
output is persisted is the optional [rollback store](/usage/rollback/), which is
**encrypted at rest** (S3 SSE by default; an encrypted volume for the file store) —
so a decrypted `Secret` never lands in plaintext on disk. See
[encryption at rest](/usage/rollback/#encryption-at-rest).

## Cloud KMS

SOPS files encrypted with a cloud KMS key (AWS KMS, GCP KMS, Azure Key Vault, or
HashiCorp Vault) decrypt through the **controller's ambient credentials** — e.g. an
IRSA role on EKS, wired via `serviceAccount.annotations`. No in-cluster key Secret
is needed, so `secretRef` may be omitted for a KMS-only `StageSet`:

```yaml
spec:
  decryption:
    provider: sops          # secretRef omitted; KMS uses the controller's identity
```

One consequence to weigh in a multi-tenant cluster: unlike age (read under the
tenant SA), **cloud KMS uses the controller's identity**, so any `StageSet` can
decrypt a file encrypted with a KMS key the controller's role can access. This
matches Flux's `kustomize-controller`. Scope the controller's KMS grant
accordingly, or use age keys for hard per-tenant isolation.

## What's supported

- **age** keys via `secretRef` — read under the tenant SA. The resource-level
  pattern (`--encrypted-regex '^(data|stringData)$'`) is the tested path.
- **PGP** keys via `secretRef` (`.asc` entries) — read under the tenant SA, pure
  Go, no `gpg` binary or keyring needed. See [PGP keys](#pgp-keys).
- **Cloud KMS** (AWS/GCP/Azure/Vault) via the controller's ambient credentials.
- **Encrypted files feeding a `secretGenerator`** — an encrypted `.env` (or other
  file) referenced by a kustomize `secretGenerator` is decrypted before the build,
  so the generated `Secret` carries the plaintext.
- A file with no SOPS metadata passes through untouched, so encrypted and plain
  manifests can sit side by side in one source.

## PGP keys

PGP works **tenant-scoped**, like age: put one or more armored private keys in the
`secretRef` Secret under data entries suffixed `.asc`. The data key is decrypted in
pure Go (`ProtonMail/go-crypto`) directly from those keys — **no `gpg` binary, no
GnuPG keyring, and no `GNUPGHOME`** — and the keys are read under the `StageSet`'s
`serviceAccountName`, so a tenant can only use material its ServiceAccount can read.

```bash
# export the armored private key and load it into the key Secret
gpg --export-secret-keys --armor 0xYOURFINGERPRINT > key.asc

kubectl create secret generic sops-keys \
  --namespace payments \
  --from-file=pgp.asc=key.asc
```

```yaml
spec:
  decryption:
    provider: sops
    secretRef:
      name: sops-keys      # holds the *.asc private key(s)
```

One Secret can carry both age (`*.agekey`) and PGP (`*.asc`) keys; the right one is
used per file. For a fresh setup, age is simpler and the recommended default, but an
existing PGP-encrypted repository needs no migration.
