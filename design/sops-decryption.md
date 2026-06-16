<!--
SPDX-FileCopyrightText: The stageset-controller Authors
SPDX-License-Identifier: 0BSD
-->

# Design: SOPS decryption for StageSet sources

Status: **proposed** · Target: phased (age/PGP first, cloud KMS later)

## Problem

A stage now consumes a `GitRepository`, `OCIRepository`, or `Bucket` directly, so
plain manifests in those sources roll out with no renderer in front. The one kind
those sources routinely carry that StageSet *cannot* yet handle is an encrypted
`Secret`: Flux users keep Secrets in Git as SOPS-encrypted YAML and rely on
`kustomize-controller`'s `spec.decryption` to decrypt them on apply. Without an
equivalent, StageSet can roll out everything from a Git/OCI/Bucket source **except**
Secrets — which undercuts direct-source support and blocks anyone migrating a Flux
`Kustomization` that uses SOPS.

The goal is parity with `kustomize-controller`'s SOPS contract, so existing
encrypted repositories and decryption-key Secrets work against StageSet unchanged.

## Non-goals

- Encryption. StageSet only ever *decrypts*; authors encrypt with the `sops` CLI.
- A new key-management story. We read the same key material, in the same Secret-key
  layout, that `kustomize-controller` already documents.
- Decrypting arbitrary non-Kubernetes files in the source (only the manifests that
  become applied objects are decrypted).

## API

Add an optional `spec.decryption` to `StageSet`, shaped exactly like Flux's so a
`Kustomization` translates field-for-field:

```yaml
apiVersion: stages.metio.wtf/v1
kind: StageSet
metadata:
  name: payments
  namespace: payments
spec:
  serviceAccountName: payments-deployer
  decryption:
    provider: sops              # the only provider; enum-validated
    secretRef:
      name: sops-age            # a Secret in the StageSet's namespace
  stages:
    - name: app
      sourceRef:
        kind: GitRepository
        name: payments-config
```

```go
// api/v1/stageset_types.go
type Decryption struct {
    // +kubebuilder:validation:Enum=sops
    Provider  string                    `json:"provider"`
    // +optional
    SecretRef *meta.LocalObjectReference `json:"secretRef,omitempty"`
}
```

The referenced Secret uses Flux's key conventions verbatim, so a Secret that works
with `kustomize-controller` works here:

| Secret key | Meaning |
|---|---|
| `*.agekey` | one or more age private keys (newline-separated) |
| `*.asc` | ASCII-armored PGP private key(s) |
| `sops.aws-kms` / `sops.gcp-kms` / `sops.azure-kv` / `sops.vault-token` | cloud-KMS credentials (phase 2) |

`secretRef` may be omitted when only cloud KMS is used and the controller's own
identity (IRSA / workload identity) holds the decryption grant.

### Validation

The admission webhook (`StageSetValidator`) rejects `provider` other than `sops`,
and rejects a missing `secretRef` unless a cloud-KMS provider is in play (phase 2).
The reconciler re-checks the same invariants when admission is bypassed — the
existing pattern.

## Where decryption happens — the load-bearing decision

The reconcile path is **fetch → kustomize build → postBuild substitute → apply**,
and on success a stage's rendered objects are pushed to the optional rollback store
(`storeRendered` → `RollbackStore.Put`). That store (PVC or S3) is the trap: if
decryption is folded into the rendered output, **plaintext Secrets land at rest**
in the store — a worse posture than not supporting SOPS at all.

The invariant is defended in **two complementary layers**:

1. **In-flight (free, strongest):** for resource-level SOPS Secrets, the artifact
   stays SOPS-encrypted all the way through the pipeline and is decrypted only at
   the apply chokepoint. The rollback store then holds objects still encrypted with
   the **tenant's** SOPS key — not even the controller's store can read them.
2. **At-rest (general safety net):** the rollback store is encrypted at rest
   regardless of SOPS, because it *already* persists every applied object — any
   `Secret`'s `data` included — as JSON. See
   [Rollback store at rest](#rollback-store-encryption-at-rest).

For layer 1, decryption is a discrete transform on the built object set applied at
the **single apply chokepoint**, immediately before `applier.Apply`, on both the
forward and rollback paths:

```
fetch ─▶ build ─▶ substitute ─▶ [snapshot/store: SOPS-encrypted] ─▶ decrypt ─▶ apply
                                                                       ▲
                                   rollback re-fetch/store ────────────┘ (also encrypted → decrypt → apply)
```

Because both `Apply` callers (`stageset_controller.go` forward apply and
`attemptRollback`) already funnel objects through `apply.Applier`, a decrypt step
wrapping that call covers every path with one insertion. The rollback store keeps
its bit-exact, GC-independent property *and* stays encrypted; rollback re-runs
decryption, so it needs the key Secret to still exist (documented).

### Scope of encryption styles

Two SOPS styles exist:

1. **Resource-level encrypted Secrets** — a valid Kubernetes YAML whose
   `data`/`stringData` values are ciphertext plus a `sops:` metadata block
   (`sops --encrypt --encrypted-regex '^(data|stringData)$'`). kustomize parses it
   fine; we decrypt the resulting `Secret` objects right before apply, so it flows
   through the pipeline SOPS-encrypted (layer 1 above). The dominant Flux pattern.
   **Phase 1.**
2. **Fully-encrypted files feeding generators** — an opaque encrypted file consumed
   by a kustomize `secretGenerator`. This *must* be decrypted at build time, so the
   rendered output (and therefore the store) carries plaintext. Layer 1 can't apply,
   but **at-rest store encryption makes it safe** — which is why we build that now
   rather than later. **Phase 3**, no longer blocked on a store redesign.

## Rollback store encryption at rest

This is worth doing **independent of SOPS**: whenever the optional rollback store
(`--rollback-store-*`) is enabled, `storeRendered` writes the full rendered object
set — including any `Secret.data` — to the PVC or S3 bucket as JSON. base64 is not
encryption, so today a plain Secret from a plain source already sits effectively in
plaintext at rest. (The `status.lastAppliedSnapshot` is unaffected — it stores only
URL/digest/revision coordinates, never content.)

The fix is **storage-native at-rest encryption**, not application-level crypto:

- **S3 backend** — set SSE on every `PutObject` (SSE-KMS with a configured key ARN,
  or SSE-S3). minio-go takes server-side-encryption options per object; the chart
  exposes `rollbackStore.s3.sse` (and a KMS key ref) and **defaults it on**.
- **Filesystem backend** — require the PVC to be backed by an encrypted volume
  (encrypted StorageClass / LUKS / cloud-disk encryption). The chart documents this
  as a precondition; the controller logs a warning at startup when the file store is
  enabled, pointing at the requirement.

Rationale for storage-native over an in-controller DEK: real key management,
rotation, and audit handled by the platform; no homegrown crypto path to age badly;
matches how the S3 *artifact* store and encrypted etcd already work. An app-level
envelope-encryption fallback (one DEK from a Secret, encrypt on `Put`) is possible
for stores with no native at-rest option, but is explicitly **future work** — not
worth the key-management surface unless a real portability need appears.

This layer is always-on (it's infra config, effectively free) and closes the gap
for non-SOPS Secrets and for SOPS style 2 alike.

## Decryptor implementation

No importable Flux decryptor exists (`kustomize-controller`'s lives in its
`internal/`), so add a small `internal/decryptor` built on
`github.com/getsops/sops/v3`:

- Load keys from the referenced Secret into an in-memory key service (age + PGP for
  phase 1) — the same construction `kustomize-controller` uses.
- `Decrypt(objects []*unstructured.Unstructured) ([]*unstructured.Unstructured, error)`
  walks the set, and for any object carrying a `sops` block decrypts its encrypted
  fields, returning a new slice (inputs untouched, so the caller's
  to-be-stored copy stays encrypted).
- The key Secret is read with the **tenant-impersonating client** (the same one
  used for `sourceRef`/library reads), so a tenant can only use keys its SA can
  read. Cloud KMS (phase 2) uses the controller's identity, like Flux.

## Tenancy & RBAC

- In-cluster keys: the decryption Secret is read under impersonation; no new
  controller-level RBAC — the tenant SA must be able to `get` its own Secret. In
  single-tenant `rbac.clusterAdmin` mode the controller reads it directly.
- Cloud KMS (phase 2): the controller's ServiceAccount needs the cloud grant
  (`serviceAccount.annotations` already wires IRSA); documented, not chart-default.
- The chart needs no new template for phase 1.

## Testing

- **Unit** (`internal/decryptor`): generate an age keypair in-test, encrypt a Secret
  fixture with `sops`, assert decrypt round-trips; assert a tampered MAC fails;
  assert non-`sops` objects pass through untouched; assert inputs are not mutated
  (the store-stays-encrypted guarantee).
- **Controller/envtest**: a StageSet with `spec.decryption` applies a Secret whose
  in-cluster value is the decrypted plaintext; with the rollback store enabled,
  assert the **stored** bytes are still ciphertext (the invariant, as a test).
- **Property/fuzz**: feed the decryptor malformed `sops` blocks; it must error, not
  panic or leak.
- **Store at-rest** (`internal/rollbackstore`): assert the S3 backend sets the SSE
  option on `PutObject` when configured; assert a round-trip `Put`/`Get` with SSE on
  returns the original bytes (the store API is unchanged for callers).
- **Smoke** (`hack/smoke/scenario-sops.sh`): a real kind cluster, an age key Secret,
  an encrypted Secret in a source, assert the applied Secret decrypts. Skips until
  the cluster prereqs (an age key) are planted by a `setup-sops.sh`.

## Docs

- New `usage/encryption.md` — the `spec.decryption` field, the Secret layout, the
  rollback-store-stays-encrypted note, the phase-1 scope.
- New tutorial **SOPS + age with Jsonnet artifacts** (tracked separately): author a
  Secret in Jsonnet, render it, `sops --encrypt` the rendered Secret, publish it via
  a source (Git, or a JaaS `output: source` snippet), and let StageSet decrypt on
  apply. This is the authoring flow that ties the renderer story to encryption.
- `comparisons/flux.md` gains a line: SOPS parity via `spec.decryption`.
- Production page: KMS-on-EKS note beside the existing IRSA S3 example.

## Phasing

0. **Rollback store at-rest encryption** — **shipped.** S3 SSE (default on) +
   encrypted-PVC precondition/warning + docs. Independent of SOPS; closes the
   existing plaintext-Secret-at-rest gap and unblocks phase 3.
1. **age, resource-level Secrets** — **shipped.** `internal/decryptor` (age via a
   custom in-memory key service, so no global `SOPS_AGE_KEY` state), `spec.decryption`
   API + deepcopy + CRD + webhook, key Secret read under the tenant SA, unit +
   envtest tests, docs. **Decryption is at build time** — the fetched source files
   are decrypted before kustomize, not at a post-build chokepoint — which is robust
   (kustomize never sees a half-stripped `sops` block) and uniform across both
   encryption styles; the rendered output reaching the rollback store is plaintext,
   so phase 0's at-rest encryption is the load-bearing guarantee. PGP was dropped
   from this phase: age is pure-Go and the dominant Flux key type, while PGP needs a
   gpg keyring the distroless runtime lacks.
2. **Cloud KMS** (AWS/GCP/Azure/Vault) — **shipped.** The decryptor appends the
   stock SOPS local key service after the injected age service, so KMS files resolve
   through the controller's ambient credentials (IRSA). `secretRef` is now optional
   (KMS-only needs no in-cluster key). The trade-off — KMS uses the controller's
   identity, not the tenant's — matches Flux and is documented. **PGP** is also
   shipped, tenant-scoped: armored `.asc` keys from `secretRef` are decrypted by a
   pure-Go `pgpKeyService` (ProtonMail/go-crypto), so it needs neither the gpg
   binary nor a GnuPG keyring — unlike Flux, which ships gpg in its image.
3. **Encrypted-file generators** — **shipped (no new code).** An encrypted `.env`
   feeding a kustomize `secretGenerator` is decrypted before the build, so the
   generator reads plaintext; covered by a test. Safe because phase 0 encrypts the
   store at rest.

## Open questions

- Should `spec.decryption` be per-stage rather than per-StageSet? Flux is
  per-Kustomization; per-StageSet matches that and is simpler. Per-stage override is
  easy to add later if a real case appears — parked as future work.
- Rollback when the key Secret was rotated/deleted in the window: rollback re-runs
  decryption, so it fails closed (`PreviousRevisionUnavailable`) if the key is gone.
  Acceptable and documented; an encrypted rollback store cannot avoid it.
