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
ilo bash -c 'controller-gen object:headerFile="hack/boilerplate.go.txt" paths=./api/v1/...'  # regenerate deepcopy — headerFile is REQUIRED so the SPDX header is kept (REUSE)
ilo bash -c 'controller-gen crd paths=./api/v1/... output:crd:dir=./config/crd'  # regenerate CRDs
```

**Static analysis is the standalone tools above — never golangci-lint** (banned
project-wide). The Go gate is `go vet` + `staticcheck` + `gosec` +
`govulncheck` + `gofumpt` + `arch-go`. These configs are kept **identical to the
jaas repo** so both projects lint the same way. Text linters mirror the same set: `yamllint`
(`.yamllint.yaml`), `actionlint`, `markdownlint-cli2` (`.markdownlint.yaml`),
`typos` (`.typos.toml`) — all installed in `dev/Containerfile` so the local
shell reproduces every CI gate.

The dev shell pre-stages the envtest asset bundle (kube-apiserver + etcd) and
exports `KUBEBUILDER_ASSETS`, so the envtest-backed packages run inside `ilo`
without any network access. Run a single fuzz target's seed-plus-campaign
locally with:

```shell
ilo bash -c 'go test -run=^$ -fuzz=^FuzzName$ -fuzztime=30s ./internal/<pkg>/'
```

## Architecture

- `cmd/main.go` — the manager entrypoint. `--watch-namespaces` (comma-separated,
  falls back to `STAGESET_WATCH_NAMESPACES`) scopes `Cache.DefaultNamespaces` —
  empty (default) is cluster-wide; `parseWatchNamespaces` does the split. The chart
  mirrors it as `controller.watchNamespaces` and pivots RBAC to per-namespace
  RoleBindings (the cluster-scoped VWC grant stays a ClusterRoleBinding). Tenancy
  has two models: **multi-tenant** (default) leans on `spec.serviceAccountName`
  impersonation, so the chart grants the controller only `impersonate` + reads;
  **single-tenant** sets the chart's `rbac.clusterAdmin: true` to bind the
  controller SA to `cluster-admin` (the helm-controller default-install model), so
  StageSets without `serviceAccountName` apply under the controller's own identity.
- `api/v1/` — `StageSet` + `StageInventory` types with kubebuilder annotations +
  handwritten `zz_generated.deepcopy.go`.
- `config/` — controller-gen output only: `crd/`, `rbac/role.yaml`,
  `webhook/manifests.yaml`. There is **no** `manager/` Deployment or kustomization
  here — the deployment lives in the Helm chart (metio/helm-charts), and the chart
  vendors these CRDs at each release. `kind-smoke.yml` installs the released chart
  with the locally-built image and overlays HEAD's CRDs.
- `internal/` — the reconciler and its collaborators:
  - `controller/` — the `StageSet` reconciler, conditions, webhook, tenant
    impersonation, migrations, rollback, windows, conflict handling.
  - `inventory/` + `stageinv/` — sharded ApplySet inventory, plan/diff, refs.
    If a stage's `StageInventory` is lost while its objects are still live,
    `Recorder.ReconstructFromCluster` self-heals it: the reconcile that finds no
    inventory for a stage `status` records as applied rebuilds it from the
    objects' owner (`stages.metio.wtf/{name,namespace}`) + per-stage
    (`stages.metio.wtf/stage`) labels across the current render's GVKs, emits an
    `InventoryReconstructed` event, and **defers pruning that pass** (never
    deletes against a best-effort rebuild); the next reconcile prunes normally.
    Best-effort: a kind no longer in the render isn't swept, so back the
    inventory up too. Stage names are lowercase-validated, so the label match is
    case-exact without normalisation.
  - `apply/` + `actions/` — server-side apply and the typed action executors.
  - `gate/` + `celeval/` — readiness gating and CEL expression evaluation.
  - `window/` — `updateWindows` (cron+duration / absolute ranges, IANA tz).
  - `artifact/` + `build/` — fetch ExternalArtifact tarballs and build stages.
    The fetcher enforces four independent byte caps: `MaxArchiveBytes`
    (compressed download only, 64 MiB), `MaxDecompressedBytes` (inflated gzip
    stream, 512 MiB), `MaxPerEntryBytes` (one tar entry, 16 MiB), and
    `MaxExtractedBytes` (extracted result held in memory, 64 MiB). These are not
    CLI flags.
  - `decryptor/` — SOPS decryption of a fetched source's files **before** the
    kustomize build, driven by `spec.decryption` (provider `sops`). Tenant-scoped
    **age** (`*.agekey`) and **PGP** (`*.asc`) keys from `secretRef` decrypt via
    custom in-memory key services (no global `SOPS_AGE_KEY`/no gpg binary/no
    keyring); **cloud KMS** rides the appended stock local key service via the
    controller's ambient creds (so `secretRef` is optional for KMS-only). Encrypted
    files feeding a `secretGenerator` work for free (decrypted pre-build). Wired in
    both the forward apply and the re-fetch rollback paths. **Build-time** (not a
    post-build chokepoint) is deliberate: kustomize never sees a half-stripped `sops`
    block, and both encryption styles — resource-level Secrets and generator-fed
    files — decrypt uniformly. The consequence is that the rendered output reaching
    the rollback store is **plaintext**, so the store's at-rest encryption (below) is
    the load-bearing confidentiality guarantee, not an in-flight SOPS layer. Rollback
    re-runs decryption, so it **fails closed** (`PreviousRevisionUnavailable`) if the
    key Secret was rotated or deleted in the window.
  - `rollbackstore/` — optional RWX-PVC / S3 store for bit-exact rollback. The store
    holds rendered Secret data, so the S3 backend defaults to server-side encryption
    (`--rollback-store-s3-sse=s3`, KMS optional) and the file backend warns to use an
    encrypted volume.
  - `metrics/` + `webhook/` — Prometheus metrics and the stage-gate webhook.

## Testing

Four distinct test layers, each living in a different place and running at a
different time. Keep new tests in the layer that fits — a pure-logic check does
not belong behind envtest, and a webhook contract cannot be exercised without a
real apiserver.

- **Pure unit tests** — most `*_test.go` under `internal/`. No cluster, no
  external assets, deterministic. They run everywhere: plain `go test` in CI's
  Verify gate and inside the dev shell. The dependency-free packages enforced by
  `arch-go.yml` (`inventory`, `diffrender`, …) are deliberately at this layer so
  they stay cluster-free and fast.
- **envtest integration tests** — packages whose `TestMain` boots a real
  kube-apiserver + etcd via `sigs.k8s.io/controller-runtime/pkg/envtest`
  (`internal/controller`, `internal/cli`, and the `internal/apply` diff suite).
  These need `KUBEBUILDER_ASSETS` pointing at an asset bundle. The dev shell
  pre-stages one in `dev/Containerfile`, so they run under `ilo`. When the
  variable is **absent**, the package skips cleanly rather than failing:
  `internal/apply`'s `TestMain` `os.Exit(0)`s and the controller/cli suites lazily
  `t.Skip` each test — so a bare `go test` on a host without assets still passes
  green. The controller and cli suites share one envtest environment per package
  (lazily started, stopped in `TestMain`) to amortize the apiserver boot.
- **Fuzz tests** — `Fuzz*` functions across several packages (`internal/apply`,
  `internal/inventory`, `internal/diffrender`, `internal/preview`, `internal/cli`).
  Each carries inline `f.Add` seeds, so a plain `go test ./...` exercises the
  **seed corpus** as ordinary cases — the Verify gate gets fuzz coverage for free.
  Coverage-guided campaigns (`go test -fuzz=…`) run only on the scheduled Fuzz
  workflow. A fuzz target in an **envtest package** (`internal/apply`) is the
  trap to remember: `go test -fuzz` re-execs the test binary as worker processes,
  and each worker would otherwise boot its own apiserver and starve the
  coordinator. `isFuzzWorker()` (matches the `-test.fuzzworker` arg) short-circuits
  `TestMain` so workers run the cluster-free fuzz function directly without
  envtest. Any new fuzz target added to an envtest package must keep that guard
  intact.
- **kind smoke e2e** — `hack/smoke/*.sh`: pure-`kubectl` behaviour scenarios
  (`scenario-basic`, `-impersonation`, `-networkpolicy`, `-cli`, `-direct-source`,
  `-rollback-s3`, `-selfsigned-webhook`, `-scale`, `-fields`) plus CRD/backend
  setup helpers, driven by `lib.sh`. The `-selfsigned-webhook`, `-scale`, and
  `-fields` scenarios mirror the equivalents in the jaas repo (parity). They are agnostic to *how* the controller was
  deployed — the calling workflow owns that — and fake the artifact data plane
  with an in-cluster static file server (a tarball baked into a ConfigMap, served
  over HTTP, pointed at by an `ExternalArtifact` whose `status.artifact` digest
  matches), so the resolve → fetch → digest-verify → build → apply → prune
  pipeline runs deterministically with no live source-controller.

## CI

`.github/workflows/verify.yml` is the **PR gate**, split into parallel jobs:

- **Go gate** — `test` (`go build` + `go test -v -race -shuffle=on -coverprofile`),
  `lint-go` (`go vet`, `staticcheck`, `gosec`, `gofumpt`), `vulnerabilities`
  (`govulncheck`, a hard merge-blocking gate), `architecture` (`arch-go`). These
  run on plain `actions/setup-go` runners with **no `KUBEBUILDER_ASSETS`**, so
  the envtest packages skip — envtest coverage comes from the dev shell and the
  smoke gate, not Verify.
- **Text + prose + license gates** — `reuse`, `yaml` (yamllint), `github-actions`
  (actionlint), `markdown` (markdownlint-cli2), `typos`, `prose` (Vale against the
  shared `metio/vale-config` style), and `docs-lint` (builds the Hugo site — after
  `hack/gen-docs-data.sh`, which needs `helm-schema` on PATH to generate each
  chart's `values.schema.json` on the fly — then lints the rendered HTML with
  `htmltest` and the theme CSS with `biome`).
- **DCO gate** — `dco` requires a `Signed-off-by` trailer on every non-bot commit.
- **Container gate** — `container-image` builds the image and scans it with
  Trivy, hard-failing on any fixable CRITICAL/HIGH (`ignore-unfixed`).
- **`all-green`** — a single aggregate job that `needs` every job above and
  fails unless each result is `success` or `skipped`. **Mark only `all-green`
  required in branch protection**; new jobs are covered automatically. Every
  workflow in this repo follows the same single-required-aggregate convention.

`kind-smoke.yml` is the **e2e gate** and angle 1 of a two-angle strategy: the
**dev binary** (this PR's HEAD build) deployed via the **latest released chart**
(`oci://ghcr.io/metio/helm-charts/stageset-controller`), run through the shared
`hack/smoke/*.sh` scenarios. Angle 2 lives in helm-charts (`stageset-smoke.yml`):
the **dev chart** deploys the **latest released binary** and runs the same
scripts. Each angle holds one moving part and tests it against the released
counterpart, so neither couples to the other repo's `main`. The workflow runs on
every PR with no paths filter and self-gates on a `relevant` git-diff output
(binary / CRD / smoke wiring touched), and the kind matrix sweeps the newest few
`kindest/node` minors discovered at runtime (a new k8s release is tested with no
manual edit). Both the smoke jobs and the gate stay **green-by-skip until the
first stageset chart release exists**. HEAD's `config/crd/` is overlaid with
`kubectl apply --server-side` because the released chart's vendored CRDs lag HEAD.

`fuzz.yml` is a **scheduled** workflow (weekly cron, off the Monday release slot,
plus `workflow_dispatch` with a `fuzztime` input). A `discover` job greps every
`Fuzz*` target out of `internal/**/*_test.go` into a `{pkg, func}` matrix, so
**new targets are fuzzed automatically** — no hand-maintained list. Each matrix
leg runs its own coverage-guided campaign; this is best-effort coverage, **not a
release gate**. It stages `KUBEBUILDER_ASSETS` for the envtest packages' initial
coordinator pass (the `isFuzzWorker` guard keeps the re-exec'd workers
cluster-free). A discovered crasher is written to `testdata/fuzz/` and printed in
the job log; commit it as a permanent seed, after which it reproduces
deterministically in the Verify gate's seed-corpus pass.

`docs.yml` builds and publishes the Hugo site under `docs/` to gh-pages.
`verify.yml`, `docs.yml`, and `fuzz.yml` are kept **structurally identical to the
jaas repo** — align changes across both. **golangci-lint is banned project-wide**
and appears nowhere in CI.

`dashboards.yml` publishes the Grafana dashboard(s) under `dashboards/` — authored
in grafonnet, rendered through **JaaS** (the controller is not a Jsonnet renderer;
the two projects are intentionally tightly coupled — "Option B") — as a
**single-layer, multi-arch OCI image** at
`ghcr.io/metio/stageset-controller-dashboard` (`:latest` + a dated calver tag,
cosign keyless-signed), the same shape as the JOI library images so it serves as
both a Flux `OCIRepository` source and an image-volume mount. grafonnet is **not**
bundled in the image — it is supplied at render time as a `JsonnetLibrary`, so the
image is just the dashboard source (`dashboards/Containerfile` is `FROM scratch` +
one `COPY *.jsonnet /`, asserted to be exactly one layer). The dashboard exposes
`datasource`/`title`/`selector` plus the SLO knobs `window`/`availabilityTarget`/
`latencyTarget` as TLAs; the consumption flow (OCIRepository → JaaS JsonnetSnippet
with `spec.tlas` → grafana-operator `GrafanaDashboard`) is in
`docs/content/observability/dashboard.md`, the SLOs it shows in
`docs/content/observability/slos.md`. A `validate` job renders every
`dashboards/*.jsonnet` against `grafonnet@main` (catching API breaks) before any
push; PRs run `validate` only, `main` pushes publish. Mirrors jaas's `dashboards.yml`.

## Build & release

Container image: `gcr.io/distroless/static:nonroot` runtime base, built multi-arch
for **`linux/amd64,arm64,arm/v7,ppc64le,riscv64,s390x`** (the metio-wide arch
set). The builder is pinned to `$BUILDPLATFORM` and cross-compiles via Go's
`GOARCH`, so the multi-arch build needs no QEMU. `VERSION`/`COMMIT` are build args.

`release.yml` is a **calendar-based** weekly release (Monday cron;
`date +'%Y.%-m.%-d'` — so the next version is the upcoming Monday's date). It is a
**hand-rolled pipeline — no goreleaser, no GPG**:

- `prepare` computes the version and gates the whole run on the commit count
  since the last release touching `go.mod cmd internal api config Dockerfile`
  (zero commits → no release; the first release always proceeds).
- `build` is a cross-compile matrix (`CGO_ENABLED=0`, `-trimpath`, `-ldflags`
  stamping `main.version`/`main.commit`) producing **both** binaries — the
  controller daemon (`./cmd`) and the `stagesetctl` CLI (`./cmd/stagesetctl`,
  which doubles as a `kubectl-stageset` plugin) — for the six linux arches plus
  windows and darwin on amd64/arm64. Each is archived: `tar.gz` on linux/darwin,
  `zip` on windows.
- `container` builds the multi-arch image and pushes
  `ghcr.io/metio/stageset-controller:{latest,<version>}` (SBOM + provenance on),
  then **cosign keyless** `sign`s it (Fulcio OIDC, `id-token: write`).
- `github` (gated on a green `container`, so a release never points at a
  non-existent image) computes one `SHA256SUMS` over every archive, **cosign
  keyless** `sign-blob`s it, and publishes the GitHub release/tag with verify
  instructions in the notes.

There is **no chart publish** here — the chart lives in the
[helm-charts](https://github.com/metio/helm-charts/tree/main/charts/stageset-controller)
monorepo. The handshake is the **bare calendar tag**: helm-charts' `vendor-crds.sh`
fetches `config/crd/` from this repo at the release tag, so the chart's vendored
CRDs always trace to a published controller release.

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

## Documentation site

`docs/` is a [Hugo](https://gohugo.io/) site published to
`https://stageset.projects.metio.wtf/` (gh-pages, via
`.github/workflows/docs.yml`). It uses the shared **metio-hugo-theme** pinned as a
**git submodule** at `docs/themes/metio` (`theme = "metio"` in `docs/hugo.toml`);
Renovate's `git-submodules` manager keeps it current. This mirrors every metio
project (`<project>.projects.metio.wtf`, theme submodule, deploy workflow) — there
are no project-local theme layouts.

GitHub Pages **must** be configured "Deploy from a branch → `gh-pages` / root":
the `docs.yml` deploy publishes the built Hugo site there with a `.nojekyll` marker
and writes the `CNAME` via the action's `cname:` parameter. **Never** set the custom
domain or create a `CNAME` through the Pages UI — doing so while the source is
`main` repoints Pages at `main`, which has no `.nojekyll`, so GitHub serves a Jekyll
build of `README.md` instead of the site (and commits a stray root `CNAME` to `main`
that then fails the REUSE gate). The domain lives in the Pages setting plus the
`gh-pages` `CNAME`; `main` carries no `CNAME` file.

The site is **end-user documentation**, authored under `docs/content/`:
`installation/`, `usage/` (one worked example per feature), `cli/` (one page per
`stagesetctl` subcommand), `api/` (a detailed field-by-field reference per CR),
and `comparisons/` (vs Helm/Kustomize/Flux/Tanka). The `docs/runbooks/` markdown
stays in place — it's pinned by the `conditions_test` drift gate and the files
double as GitHub-rendered docs — and is surfaced into `content/runbooks/` via a
`[module.mounts]` entry in `docs/hugo.toml`; `ignoreFiles = ['README\.md$']` keeps
the GitHub-facing README out of the render. The desktop nav is an explicit
`[menu.main]` tree — the theme only renders menu entries that have children. There
are **no** design/decision docs in the tree: that material was folded into the
end-user docs (every StageSet YAML example is treated as a designed artifact —
keep them beautiful).

Build/preview locally with ilo argument files; `--no-rc` bypasses the Go-shell
`.ilo.rc` that would otherwise clash:

```shell
ilo --no-rc @dev/website   # one-shot build into docs/public/
ilo --no-rc @dev/serve     # live server on :1313
```

Two website linters are pre-staged in the Go `dev/Containerfile` (alongside markdownlint), so they run in the default `ilo bash` shell — no `--no-rc`. Build the site first, then:

```shell
ilo bash -c 'htmltest'                          # rendered HTML: dead internal links, missing alt, broken anchors (.htmltest.yml)
ilo bash -c 'cd docs/themes/metio && biome lint'   # theme CSS (run from theme dir, no path; biome v2 treats its biome.json as the project root and files.includes scopes the run)
```

`htmltest` reads `.htmltest.yml` (rooted at `docs/public`, external links off for a deterministic offline gate — flip `CheckExternal` to verify outbound URLs). The CSS lives in the theme submodule, so `biome` is configured by `biome.json` *in the theme repo* (it excludes the vendored `normalize.css` / `syntax.css`); fix CSS findings there, not in the submodule checkout.

### Generated docs data (flags + chart values)

Two data-driven reference pages are built from source rather than hand-maintained,
so they can never drift from the runtime contract or the chart schema:

- **Configuration reference** (`docs/content/installation/configuration.md`)
  renders one `{{< flag-table group="…" >}}` per subsystem. The controller's own
  CLI flags live in `internal/cliflags/` — `Register(fs *flag.FlagSet)` declares
  every flag on a stdlib `flag.FlagSet`, co-locates each with its documentation
  group (`groupByName`, read via `GroupOf`; group order via `Groups()`), and
  returns a `*Flags` of value pointers. `cmd/main.go` calls
  `cliflags.Register(flag.CommandLine)` and dereferences `c.*` after `flag.Parse`.
  The package is importable (not `main`) precisely so `hack/flaggen` can build the
  same FlagSet and emit `docs/data/flags.json`. The controller-runtime **zap flags
  stay bound in `cmd/main.go`** via `opts.BindFlags` and are documented as prose in
  the Logging section — flaggen documents only the controller's own flags. Go's
  stdlib `flag` exposes no type, so the flag table has no Type column.
- **Helm chart values** (`docs/content/installation/helm-values.md`) renders
  `{{< helm-values data="helm-values" >}}` from `docs/data/helm-values.json`, which
  `hack/flatten-schema.jq` flattens from the stageset-controller chart's
  `values.schema.json` (fetched from helm-charts' `main`).

`hack/gen-docs-data.sh` regenerates both files (flaggen → `docs/data/flags.json`,
schema fetch+flatten → `docs/data/helm-values.json`); run it in the ilo Go shell
before building the site. Both outputs are **gitignored** — the published site is
always generated from source. The shortcodes degrade to nothing when the data file
is absent, so a bare `hugo` on a clean checkout never errors. `docs.yml` runs on a
**daily** cron (so a chart change reaches the site with no cross-repo trigger) and
runs `gen-docs-data.sh` before the Hugo build; its push/PR `paths` include the
flag-source files (`cmd/main.go`, `internal/cliflags/**`, `hack/flaggen/**`,
`hack/flatten-schema.jq`, `hack/gen-docs-data.sh`) so a flag change rebuilds the
site.

## Licensing / REUSE

0BSD, REUSE-compliant. Every file carries an SPDX header (Go via `//`, YAML/shell
via `#`, markdown via `<!-- -->`) or a `REUSE.toml` glob (which covers `docs/**`,
`.gitmodules`, and the `dev/website` / `dev/serve` ilo argument files). The `reuse`
workflow enforces it. The `docs/themes/metio` submodule is a separate CC0 repo,
outside this repo's REUSE scope.
