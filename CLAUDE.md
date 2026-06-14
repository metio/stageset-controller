<!--
SPDX-FileCopyrightText: The stageset-controller Authors
SPDX-License-Identifier: 0BSD
-->

# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

> Note: the on-disk directory is `flux-stageset-controller`, but the Go module
> and the project name are **`stageset-controller`** (`github.com/metio/stageset-controller`).
> Use that name; do not call it `flux-stageset-controller`.

## Overview

A [Flux](https://fluxcd.io)-compatible Kubernetes controller for **ordered,
gated, multi-stage deployments**. A `StageSet` deploys a sequence of stages,
each built from an `ExternalArtifact` (RFC-0012) source, pinning **all** artifact
revisions at the start of a run so every stage applies a consistent snapshot —
something chained `Kustomization` + `dependsOn` cannot offer.

Two CRDs at **`stages.metio.wtf/v1`**:

| Kind | Purpose |
|---|---|
| `StageSet` | the user-facing spec: ordered stages, gates, actions, windows, migrations, rollback |
| `StageInventory` | the sharded, ApplySet-compliant record of what each stage applied (cross-stage ownership transfer, reverse-order pruning) |

Headline capabilities: gated progression (kstatus + CEL `healthCheckExprs`),
per-stage pruning with reverse-order teardown, typed pre/post/onFailure
**actions** (`patch`/`http`/`wait`/`job`/`delete`/`apply` — a declarative
replacement for Helm hook Jobs, gated by a per-revision ledger), Flagger
canary-gated stages, **producer-aware references** (a stage names the object that
produced its artifact — e.g. a JaaS `JsonnetSnippet` — resolved via the RFC-0012
`spec.sourceRef` back-pointer), multi-tenancy parity with kustomize-controller
(`spec.serviceAccountName` impersonation + `spec.kubeConfig` remote-cluster
apply), versioned `spec.migrations` (version-gated action ladders that run once
when a version boundary is crossed), `rollbackOnFailure`, and time-based
delivery (`spec.updateWindows`).

## Common commands

No host toolchain; commands run in a containerized dev shell driven by
`dev/Containerfile`. A `.ilo.rc` at the repo root supplies the args (it mounts
the Go module cache and sets `GOSUMDB=off`), so the short form works:

```shell
ilo bash -c 'go build ./...'
ilo bash -c 'go vet ./...'
ilo bash -c 'staticcheck ./...'                       # checks=all via staticcheck.conf
ilo bash -c 'gofumpt -l .'                            # strict formatting (empty == clean)
ilo bash -c 'gosec ./...'                             # security scanner
ilo bash -c 'arch-go'                                 # architecture rules (arch-go.yml)
ilo bash -c 'govulncheck ./...'
ilo bash -c 'go test -count=1 -race -shuffle=on -cover ./...'   # full suite (envtest assets prestaged in the image)
ilo bash -c 'controller-gen object paths=./api/v1/...'         # regenerate deepcopy
ilo bash -c 'controller-gen crd paths=./api/v1/... output:crd:dir=./config/crd'  # regenerate CRDs
ilo bash -c 'kubeconform -ignore-missing-schemas -summary config/samples/'
```

**Static analysis is the standalone tools above — never golangci-lint** (banned
project-wide). The Go gate is `go vet` + `staticcheck` + `gofumpt` + `gosec` +
`arch-go` + `govulncheck`. These configs are kept **identical to the jaas repo**
so both projects lint the same way.

## Architecture

- `cmd/main.go` — the manager entrypoint.
- `api/v1/` — `StageSet` + `StageInventory` types with kubebuilder annotations +
  handwritten `zz_generated.deepcopy.go`.
- `config/` — kubebuilder-style manifests (`crd/`, `manager/`, `rbac/`,
  `webhook/`, `samples/`). Unlike jaas, this repo ships deployable raw manifests
  here; `kind-smoke.yml` applies them with the locally-built image.
- `internal/` — the reconciler and its collaborators:
  - `controller/` — the `StageSet` reconciler, conditions, webhook, tenant
    impersonation, migrations, rollback, windows, conflict handling.
  - `inventory/` + `stageinv/` — sharded ApplySet inventory, plan/diff, refs.
  - `apply/` + `actions/` — server-side apply and the typed action executors.
  - `gate/` + `celeval/` — readiness gating and CEL expression evaluation.
  - `window/` — `updateWindows` (cron+duration / absolute ranges, IANA tz).
  - `artifact/` + `build/` — fetch ExternalArtifact tarballs and build stages.
  - `rollbackstore/` — optional RWX-PVC / S3 store for bit-exact rollback.
  - `metrics/` + `webhook/` — Prometheus metrics and the stage-gate webhook.

## Build & release

Container image: `gcr.io/distroless/static:nonroot` runtime base, built multi-arch
for **`linux/amd64,arm64,arm/v7,ppc64le,riscv64,s390x`** (the metio-wide arch
set). The builder is pinned to `$BUILDPLATFORM` and cross-compiles via Go's
`GOARCH`, so the multi-arch build needs no QEMU. `VERSION`/`COMMIT` are build args.

`release.yml` is a **calendar-based** weekly release (`date +'%Y.%-m.%-d'`):
`prepare` (version, gated on commit count) → `github` (GitHub release/tag with
cosign-verify notes) → `container` (multi-arch push to
`ghcr.io/metio/stageset-controller` + cosign keyless). No standalone CLI binaries
(this is a controller, not a CLI) and **no chart publish** — the chart lives in
the [helm-charts](https://github.com/metio/helm-charts/tree/main/charts/stageset-controller)
monorepo. The bare calendar tag is what helm-charts' `vendor-crds.sh` fetches
CRDs against.

## Conventions & traps

- **The producer-aware JaaS integration is load-bearing.** A stage resolving a
  `JsonnetSnippet` through the `ExternalArtifact.spec.sourceRef` back-pointer
  (`{apiVersion: jaas.metio.wtf/v1, kind: JsonnetSnippet, name}`) is a public
  contract with jaas. Do not break the reverse-resolution of that triple.
- **The webhook is always-on** (no operator toggle), so its chart defaults
  `certMode: self-signed` — a cert-manager default would fail a `required`
  issuer at render time with no prerequisite installed.
- The ClusterRole needs `events` (for the EventRecorder) and `leases` (for
  leader election) even though `controller-gen` doesn't emit those from the
  reconciler's RBAC markers — they're added deliberately.
- **Comment style:** describe the *current* code's intent/invariants, not its
  history. No "previously…/we used to…", no references to review docs or
  bug-tracker IDs. Write as the maintainer, not an outside auditor.

## Licensing / REUSE

0BSD, REUSE-compliant. Every file carries an SPDX header (Go via `//`, YAML/shell
via `#`, markdown via `<!-- -->`) or a `REUSE.toml` glob. The `reuse` workflow
enforces it.
