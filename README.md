<!--
SPDX-FileCopyrightText: The stageset-controller Authors
SPDX-License-Identifier: 0BSD
-->

# stageset-controller

A [Flux](https://fluxcd.io)-compatible Kubernetes controller for **ordered,
gated, multi-stage deployments**. A `StageSet` deploys a sequence of stages,
each built from an [`ExternalArtifact`](https://fluxcd.io) (RFC-0012)
source, with:

- **One CR, one status, one revision set** — all artifact revisions are
  pinned at the start of a run, so every stage applies a consistent
  snapshot (something chained `Kustomization` + `dependsOn` cannot offer).
- **Gated progression** — a stage completes only when its ready checks pass
  (kstatus, CEL expressions compatible with `healthCheckExprs`).
- **Per-stage pruning** with reverse-order teardown of removed stages,
  cross-stage ownership transfer, and sharded
  [ApplySet](https://kubernetes.io/blog/2023/05/09/introducing-kubectl-applyset-pruning/)-compliant
  inventory.
- **Typed pre/post/onFailure actions** (`patch`, `http`, `wait`, `job`,
  `delete`, `apply`) — a declarative replacement for Helm hook Jobs. Because
  actions are gated by a per-revision ledger, an `apply`+`delete` pair stands a
  transient resource (e.g. a maintenance-page pod) up only for the duration of
  a rollout, with `patch` flipping the Ingress around it.
- **Flagger integration** — Canary-gated stages plus a stage gate webhook
  for migration-before-promotion guarantees.
- **Producer-aware references** — a stage can name the object that
  produced its artifact (e.g. a [JaaS](https://github.com/metio/jaas)
  `JsonnetSnippet`) instead of the `ExternalArtifact` itself, resolved via
  the RFC-0012 `spec.sourceRef` back-pointer.
- **Multi-tenancy parity with kustomize-controller** — per-StageSet
  `spec.serviceAccountName` impersonation and `spec.kubeConfig` remote-cluster
  apply, which compose, plus `--no-cross-namespace-refs` gating.
- **Versioned migrations** — `spec.version` (inline or read from an artifact)
  plus `spec.migrations`, version-gated action ladders that run once when a
  version boundary is crossed, anchored before a stage, with baselining and
  downgrade refusal.
- **`rollbackOnFailure`** — restores the last-applied artifact revisions when a
  run fails (best-effort via producer retention; bit-exact and
  GC-independent with an optional RWX-PVC or S3 rollback store).
- **Time-based delivery** — per-StageSet `spec.updateWindows` (Allow/Deny,
  recurring cron+duration or absolute date ranges, IANA timezones) gate when new
  revisions roll out; `windowScope: All` is a hard freeze. A held-but-deployed
  StageSet stays Ready (`status.pendingUpdate` shows what's waiting); the
  `stages.metio.wtf/update-now` annotation forces a rollout through.

> **Status: v1 implemented.** The reconciler runs the full state machine
> (resolve → pin → fetch → build → apply → prune → verify), typed actions,
> `dependsOn` gating, finalizer teardown, events, metrics, and the Flagger
> gate endpoint. Coverage spans unit, envtest, and a kind smoke test. The
> [design document](docs/content/design/stageset.md) records the complete
> rationale and the reserved (post-v1) API surface.

## Repository layout

| Path                  | Contents                                                          |
|-----------------------|-------------------------------------------------------------------|
| `api/v1/`             | `StageSet` and `StageInventory` types (`stages.metio.wtf/v1`)     |
| `internal/inventory/` | Dependency-free inventory core: IDs, prune planning, sharding, ApplySet metadata — 100% test coverage |
| `internal/controller/`| The `StageSet` reconciler, validating webhook, conditions, and target-cluster (impersonation + kubeConfig) wiring |
| `internal/{artifact,build,apply,stageinv,actions,celeval,gate,metrics}/` | Resolve/fetch, kustomize build, SSA apply, inventory recording, typed actions, CEL eval, Flagger gate, metrics |
| `cmd/`                | Manager entrypoint with all controller flags                      |
| `config/`             | Generated CRDs + RBAC, controller `Deployment` (`config/manager/`), samples |
| `docs/`               | Hugo design site (`docs/content/design/`) and operator [runbooks](docs/runbooks/) (`docs/runbooks/`) |

## Getting started

Prerequisites: [Go](https://go.dev) 1.22+, `make`,
[Hugo](https://gohugo.io) (docs only),
nothing else — every analysis tool runs via `go run`.

```sh
# Run the test suite (unit + envtest):
make test

# Run every local check (vet, lint, tests, architecture, REUSE, YAML):
make verify

# Regenerate DeepCopy code and CRDs after editing api/v1:
make generate manifests

# Work on the docs:
make docs-serve
```

envtest-based controller tests need the apiserver+etcd binaries; the dev
container pre-stages them via `KUBEBUILDER_ASSETS`. A kind smoke test
(`.github/workflows/kind-smoke.yml`) exercises the full pipeline — deploy,
resolve an `ExternalArtifact`, apply, prune, and `serviceAccountName`
impersonation — in a real cluster.

`make help` lists all targets.

## Design

The complete design — motivation, API, reconciliation model, inventory
modes, Flagger integration, stage actions, and every resolved decision with
its rationale — lives in
[`docs/content/design/stageset.md`](docs/content/design/stageset.md) and is
served by the Hugo site. Read it before touching the reconciler: the
controller TODOs reference its sections by name.

Controller flags:

| Flag                       | Default  | Purpose                                          |
|----------------------------|----------|--------------------------------------------------|
| `--inventory-mode`         | `hybrid` | `entries`, `hybrid`, or `applyset` inventory     |
| `--inventory-shard-cap`    | `5000`   | Max entries per StageInventory shard             |
| `--allowed-action-hosts`   | _(none)_ | Host globs permitted for `http` actions (SSRF guard); repeatable |
| `--no-cross-namespace-refs`| `false`  | Deny cross-namespace `sourceRef`/`dependsOn`     |
| `--runbook-base-url`       | _(none)_ | URL prefix appended to actionable Ready messages as `(runbook: <base>/<reason>.md)` |
| `--enable-webhook`         | `true`   | Run the validating admission webhook for `StageSet` |
| `--webhook-cert-mode`      | `cert-manager` | Webhook TLS provisioning: `cert-manager` (Secret-mounted cert) or `self-signed` (in-pod CA+cert, patches the VWC caBundle, hot-reloads + rotates, HA-safe). Self-signed also takes `--webhook-cert-dir`, `--webhook-port`, `--webhook-cert-validity`, `--webhook-service-name`, `--webhook-service-namespace`, `--webhook-validating-config-name` |
| `--gate-bind-address`      | `:8082`  | Read-only Flagger stage-gate endpoint; empty disables |
| `--metrics-bind-address`   | `:8080`  | Prometheus metrics endpoint                      |
| `--health-probe-bind-address` | `:8081` | `/healthz` + `/readyz` probes                  |
| `--leader-elect`           | `false`  | Leader election for HA controller replicas       |
| `--rollback-store-path`    | _(off)_  | Filesystem dir (e.g. an RWX PVC mount) for bit-exact, GC-independent rollback. Use RWX for HA replicas |
| `--rollback-store-s3-*`    | _(off)_  | S3-compatible store for the same: `-endpoint`, `-bucket`, `-prefix`, `-region`, `-use-ssl`, `-access-key`, `-secret-key`, `-session-token`, `-anonymous`. Empty static creds engage the IAM/IRSA chain. Mutually exclusive with `--rollback-store-path` |

## Operations

When a `StageSet` is not `Ready`, `kubectl describe stageset <name>` shows the
reason and a human-readable message. Each wire-stable reason has a
[runbook](docs/runbooks/) covering symptom, cause, diagnosis, and remediation;
set `--runbook-base-url` to a published copy of `docs/runbooks/` and the
controller links the relevant page directly in the condition message.

Multi-tenant installs scope what a `StageSet` can touch with
`spec.serviceAccountName` (the apply, prune, verify, and actions run impersonated
as that ServiceAccount) and apply to remote clusters with `spec.kubeConfig`
(a Secret-held kubeconfig); the two compose. StageInventory and status always
stay on the controller's own cluster.

**Force a reconcile** without waiting for the interval by setting the standard
Flux annotation (this is what `flux reconcile` does):

```sh
kubectl annotate --overwrite stageset <name> \
  reconcile.fluxcd.io/requestedAt="$(date +%s)"
```

The controller records the handled token in `status.lastHandledReconcileAt`.

**Drift** — an object changed or deleted out-of-band is corrected on the next
reconcile (server-side apply re-asserts desired state). When that happens on a
steady-state reconcile (no new artifact revision), the controller emits a
`DriftCorrected` Event and increments `stageset_drift_corrected_total`, so
out-of-band tampering is visible rather than silently healed.

The design rationale for the cross-cutting decisions is recorded under
[`docs/decisions/`](docs/decisions/).

## Static analysis

The project treats static analysis as part of the build, not an
afterthought. CI (`.github/workflows/verify.yml`) runs:

| Tool | Scope |
|------|-------|
| `go vet` (all analyzers) | Go correctness |
| [staticcheck](https://staticcheck.dev) (`checks = ["all"]`, see `staticcheck.conf`) | Bugs, simplifications, performance, style |
| [gosec](https://github.com/securego/gosec) | Security patterns (hardcoded credentials, weak crypto, injection) |
| [govulncheck](https://go.dev/security/vuln/) | Known vulnerabilities in the dependency graph |
| [arch-go](https://github.com/arch-go/arch-go) | Architecture rules — notably: `internal/inventory` must stay stdlib-only |
| [gofumpt](https://github.com/mvdan/gofumpt) (`make fmt-check`) | Strict formatting |
| [REUSE](https://reuse.software) | License and copyright metadata on every file |
| [yamllint](https://yamllint.readthedocs.io) | All YAML |
| [actionlint](https://github.com/rhysd/actionlint) | GitHub Actions workflows |
| [markdownlint](https://github.com/DavidAnson/markdownlint-cli2) | All Markdown |
| [typos](https://github.com/crate-ci/typos) | Spelling |
| [kubeconform](https://github.com/yannh/kubeconform) | Sample manifests |

Dependabot keeps Go modules and Actions current (weekly).

## License

[0BSD](LICENSES/0BSD.txt) — see [REUSE.toml](REUSE.toml) for per-file
metadata. The project is [REUSE](https://reuse.software) compliant.
