---
title: "stagesetctl: A CLI for StageSets"
weight: 2
---

# Design: `stagesetctl` — A CLI for StageSets

| | |
|---|---|
| **Status** | Draft v1 |
| **Binary** | `stagesetctl` (also usable as `kubectl stageset`) |
| **Kind operated on** | `StageSet` (`stages.metio.wtf/v1`) |
| **Author** | Seb |
| **Last updated** | 2026-06-15 |

## Summary

`stagesetctl` is a client-side companion to the StageSet controller. It gives
operators the three things a GitOps deployment tool is missing without a CLI:

1. **`diff`** — *the* headline feature. Preview exactly what a `StageSet`
   would change in the cluster **before** the controller applies it, the same
   way `flux diff kustomization`, `tk diff`, and `kubecfg diff` do for their
   respective worlds. Render is computed through the controller's **own**
   packages, so the preview matches what the controller will actually apply.
2. **`reconcile`** — force an out-of-band reconcile of a whole `StageSet`
   (the Flux `reconcile.fluxcd.io/requestedAt` mechanism the controller
   already honors), optionally re-fetching sources and/or bypassing update
   windows; **or force a single stage** to re-run its actions.
3. **`build`** and **`get`** — render a stage's manifests to stdout (offline
   capable), and print a human-readable status of a `StageSet` (phases,
   pending updates, windows, migrations) that `kubectl get` cannot.

The CLI lives in this repository and **imports the controller's render path
directly** (`internal/artifact`, `internal/build`, `internal/inventory`,
`internal/apply`). This is the single most important architectural decision:
there is exactly one render implementation, shared by the controller and the
preview, so a `diff` can never drift from what gets applied. This mirrors how
Flux shares `internal/build` between `flux build`, `flux diff`, and the
kustomize-controller.

## Goals

- A `diff` whose output is faithful to what the controller applies, including
  mutating-webhook and apiserver-defaulting effects (server-side dry-run), and
  which surfaces **prunes** (objects that would be deleted) as well as
  creates/updates.
- A diff that is safe to drop into CI logs (Secret values masked by default)
  and scriptable as a GitOps gate (`diff(1)` exit-code convention).
- Force-reconcile for a whole `StageSet` and for a single stage, both built on
  the same compare-token-to-status idempotency mechanism Flux uses.
- A tool that is **also** a `kubectl` plugin, with the standard connection
  flags (`--kubeconfig`, `--context`, `--namespace`) operators already know.
- Tests at every layer: pure-unit (diff rendering, masking, exit codes), engine
  (preview against a fake/dry-run cluster), envtest integration (the whole
  command via a `Run` seam), golden output, and a kind smoke scenario.

## Non-goals

- **Not an applier.** `stagesetctl` never mutates workload objects in the
  cluster. The only writes it performs are *annotations on the `StageSet`
  itself* (to request a reconcile) — the controller does all real applying.
  This keeps the CLI's blast radius tiny and its RBAC honest.
- **Not a Jsonnet renderer.** Snippets are rendered by their producer (JaaS);
  the `StageSet` consumes the produced `ExternalArtifact`. The CLI fetches the
  already-produced artifact; it does not evaluate Jsonnet.
- **No new server.** No daemon, no controller changes beyond the small,
  well-scoped single-stage-reconcile mechanism described below.

## Naming and packaging

One binary, `stagesetctl`. When placed on `PATH` as `kubectl-stageset`, the
same binary is invoked as `kubectl stageset …` (kubectl's plugin convention).
The root command derives its displayed name from `filepath.Base(os.Args[0])`
so help text reads correctly under both invocations (`stagesetctl diff …` vs
`kubectl stageset diff …`).

Build target: a second `main` package at `cmd/stagesetctl/main.go`, separate
from the manager's `cmd/main.go`. The `Dockerfile`/release pipeline can ship it
as an additional artifact later; v1 only needs `go build ./cmd/stagesetctl`.

## Framework and dependencies

- **cobra + pflag** for the command tree (already indirect deps; promoted to
  direct). GNU-style flags, subcommands, shell completion, generated help —
  the ecosystem standard (flux, kubectl, argo, tanka).
- **`k8s.io/cli-runtime/pkg/genericclioptions`** (already indirect) for
  `ConfigFlags` (`--kubeconfig`, `--context`, `--cluster`, `--user`,
  `-n/--namespace`, `--as`/impersonation) and `IOStreams` (in/out/err). This
  gives us the exact connection-flag surface operators expect from a kubectl
  plugin, for free, and makes the command testable by injecting buffers.
- **`fluxcd/pkg/ssa`** (already direct) for the server-side dry-run diff.
- **`github.com/wI2L/jsondiff`** and/or **`github.com/pmezard/go-difflib`**
  (both already indirect) for computing the textual unified diff between the
  live and merged objects. `go-difflib` produces standard unified-diff output;
  we render YAML on both sides and diff line-wise.
- **`golang.org/x/term`** (already indirect) for TTY detection (`--color=auto`).

**No new modules are downloaded** — every dependency above is already in
`go.sum` as an indirect dep of the manager. `go mod tidy` simply promotes them.

## Architecture and the testability seam

`cmd/stagesetctl/main.go` is a 4-line shell, mirroring the JaaS `run(...) int`
convention (which the manager's `main.go` does *not* follow, but which is the
right pattern for a CLI):

```go
func main() {
    streams := genericclioptions.IOStreams{In: os.Stdin, Out: os.Stdout, ErrOut: os.Stderr}
    os.Exit(cli.Run(context.Background(), streams, os.Args[1:]))
}
```

All logic lives in a new `internal/cli` package. `cli.Run(ctx, streams, args)
int` builds the cobra root command bound to `streams`, executes it, and maps
errors to exit codes. Because `Run` takes its streams and args as parameters
and never calls `os.Exit` itself, tests drive whole commands with in-memory
buffers and assert on stdout/stderr/exit code — the same seam JaaS uses for
`main_test.go` / `examples_test.go`.

### Exit-code contract (wire-stable, documented)

Following `diff(1)` / `kubectl diff` / `argocd app diff` (not kapp's inverted
scheme), so existing CI scripts behave as expected:

| Code | Meaning |
|---|---|
| `0` | Success; for `diff`, **no** changes. |
| `1` | For `diff`, **changes present**. (Other commands: not used for success.) |
| `2` | Usage / flag error. |
| `>2` | Runtime error (connection, RBAC, render failure). |

`diff` accepts `--exit-code=false` (default `true`) to always return `0` on a
clean run regardless of drift — for pipelines that report drift without
failing. "Changes present" (`1`) and "tool error" (`>2`) are deliberately
distinct codes so a pipeline can tell drift from breakage (ArgoCD's lesson).

## Connection, scoping, and reachability

Standard `genericclioptions.ConfigFlags` provide kubeconfig/context/namespace.
A positional `NAME` selects the `StageSet`; `-n/--namespace` (or the
kubeconfig's current namespace) scopes it.

**Reachability of the artifact.** The render path needs the `ExternalArtifact`
tarball, served from an in-cluster storage endpoint that an out-of-cluster CLI
may not reach. Three modes, in order of preference:

1. **In-cluster / reachable URL** (default): the CLI resolves the
   `ExternalArtifact`, reads `status.artifact.url`, and fetches it directly —
   works when run in-cluster or when the storage URL is routable.
2. **`--source-dir DIR`** (offline / unreachable): supply a locally-rendered
   artifact tree (e.g. produced by the JaaS local renderer or `tar -xzf`),
   keyed `--source-dir <stage>=<path>` (repeatable) or a single `--source-dir
   <path>` applied to all stages. Mirrors `flux diff --local-sources`. Lets a
   `diff` run with no network path to storage, and lets `build` run fully
   offline.
3. **Port-forward** (documented, not automated in v1): operators
   `kubectl port-forward` the storage service and pass the local URL.

The fetcher's SSRF guard (`URLValidator`/`IPValidator`) is configured
permissively for the CLI (the operator is dialing their own cluster), matching
how the controller's own tests relax it for httptest.

## `diff` — the core command

```text
stagesetctl diff NAME [-n NS] [--stage NAME ...] [flags]
```

### Engine

For each stage (or the subset named by `--stage`, repeatable):

1. **Resolve** the stage's `sourceRef` to a ready `ExternalArtifact` via
   `artifact.Resolver.Resolve(ctx, client, ref, ns)` — the *same* resolver the
   controller uses, including the RFC-0012 producer back-pointer. (Skipped when
   `--source-dir` supplies the files.)
2. **Fetch** the tarball via `artifact.Fetcher.Fetch` (digest-verified), or read
   `--source-dir`.
3. **Resolve postBuild vars** (ConfigMap/Secret lookups) the same way the
   reconciler does, so `${var}` substitutions render identically. Read with the
   user's own credentials (or impersonated SA — see below).
4. **Build** with `build.Build(files, build.Options{Path, Patches}, vars)` →
   `[]*unstructured.Unstructured`. This is byte-for-byte the controller's build.
5. **Dry-run diff** each object via a new `apply.Applier.Diff` (below) that
   wraps `ssa.ResourceManager.Diff` — a **server-side dry-run apply** using the
   **same field manager** (`stageset-controller`) the controller writes with,
   so SSA field-ownership and the resulting merge match production. Returns, per
   object: action (`created` / `configured` / `unchanged`) plus the `existing`
   and `merged` unstructured objects.
6. **Prunes**: read the stored `StageInventory` records
   (`stageinv.Recorder.StageRecords`), build the desired records from step 4,
   and run `inventory.ComputePlan(previous, desired)`. Objects in
   `PrunePerStage` and entries of `RemovedStages` are rendered as **deletions**
   (full object shown as removed). This is what makes the StageSet diff complete
   — it shows not just what changes but what *goes away*, which a plain
   per-object apply-diff cannot.

### Output

Per object, grouped by stage, in apply/prune order:

- A header line: `<action> <kind>/<name> [namespace]` where action ∈
  `create | configure | delete | unchanged`.
- For `configure`/`delete`: a **unified diff** of the normalized YAML
  (`existing` vs `merged`; for deletes, `existing` vs empty), `+`/`-` gutters,
  colorized on a TTY.
- `unchanged` objects are suppressed unless `--show-unchanged`.

After the object diffs, two informational sections make the diff a true
"what will happen on the next run" preview:

- **Actions to run** — per stage, the `pre`/`post`/`onFailure` actions a
  rollout would execute, with each action's type and a short detail
  (`patch ConfigMap/maintenance`, `http POST https://…`). Actions the
  idempotency ledger has already satisfied at the rendered revision are
  omitted, so the section shows exactly what will fire. A local
  (`--source-dir`) render cannot consult the ledger, so it lists every action.
- **Migrations to run** — the migrations the next run will execute, taken from
  the controller's own `status.pendingMigrations` so the section appears **iff**
  migrations will actually run, each with its version boundary, anchor stage,
  and action count.

A trailing **change-set summary** in kapp's scannable style, keyed by action
and (optionally) stage, printed last so it is always the final line:

```text
Summary: 3 to create, 1 to configure, 2 to delete, 11 unchanged  (stage: canary)
```

**Noise suppression** — before diffing, strip server-populated /
controller-managed fields that carry no authored intent:
`metadata.managedFields`, `resourceVersion`, `generation`, `uid`,
`creationTimestamp`, `selfLink`, the `status` subtree, and
`kubectl.kubernetes.io/last-applied-configuration`. (fluxcd `ssa/normalize`
handles some of this; we apply an explicit strip for the rest so output is
stable and tested.)

**Secret masking** — Secret `data`/`stringData` values are masked by default
with a placeholder that still signals a change and correlates identical values
(kapp style: `<-- value not shown (#N)`). `--show-secrets` opts out. Masking is
applied to *both* sides before rendering so a value change shows as
mask-change, never plaintext. (We mask **all** Secret values by default — the
kubectl/kapp/argo majority — rather than Flux's SOPS-only approach, because
diff output lands in CI logs.) Masking lives in one tested function used by
`diff` and `build` alike; this is security-critical (cf. ArgoCD CVE-2025-23216,
where a server-side-diff path missed masking).

**Color** — `--color=auto|always|never` (default `auto`). `auto` enables color
only on a TTY (`x/term.IsTerminal`) and honors `NO_COLOR`. Color is additive
over the `+`/`-` gutter so piped output stays meaningful.

### Flags

| Flag | Default | Purpose |
|---|---|---|
| `--stage NAME` (repeatable) | all | Diff only the named stage(s). |
| `--source-dir [STAGE=]PATH` (repeatable) | — | Local artifact tree; skip fetch. |
| `--server-side` | `true` | Server-side dry-run apply diff. `false` → client-side render-vs-live (no `update`/`patch` RBAC needed, less faithful). |
| `--as-tenant` | `false` | Impersonate the StageSet's `spec.serviceAccountName` (the identity the controller renders/applies as), so the diff reflects the tenant's RBAC. Default uses the caller's own identity. |
| `--show-secrets` | `false` | Reveal Secret values. |
| `--show-unchanged` | `false` | Include unchanged objects. |
| `--color` | `auto` | `auto`/`always`/`never`. |
| `--exit-code` | `true` | `false` → always exit `0`. |
| `--prune` | `true` | Include would-be-deletions in the diff. |

> **RBAC note (documented prominently):** server-side dry-run is a PATCH, so it
> needs `update`/`patch` on the target resources even though nothing persists —
> the same surprise `kubectl diff` carries. `--server-side=false` avoids it.

## `build` — render to stdout

```text
stagesetctl build NAME [--stage NAME] [--source-dir ...] [-o yaml]
```

Runs steps 1–4 of the diff engine and writes the rendered multi-doc YAML to
stdout — the StageSet analog of `flux build kustomization` / `tk show` /
`kubectl kustomize`. Fully offline with `--source-dir`. Secret-masked by
default (`--show-secrets` to reveal). Useful for inspecting exactly what a stage
produces, feeding into external diff tools, or debugging postBuild
substitution. Refuses to write to a TTY-redirect silently? No — unlike `tk
show` we simply write to stdout; `-o yaml` is the only format in v1.

## `reconcile` — force an out-of-band reconcile

```text
stagesetctl reconcile NAME [-n NS] [--stage NAME] [--with-source] [--update-now] [--wait] [--timeout DUR]
```

### Whole-StageSet

Patches `reconcile.fluxcd.io/requestedAt` on the `StageSet` with a fresh token
(RFC3339Nano timestamp) using a dedicated field manager
(`stagesetctl`), exactly as `flux reconcile` does. The controller already reads
this annotation and records `status.lastHandledReconcileAt` (verified in
`reconcile_trigger_test.go`). Because the controller SSA-applies every
reconcile, a `requestedAt` bump already re-applies the desired state — there is
no separate whole-object `forceAt` needed, and the design does not invent one.

- `--with-source` — also bump `requestedAt` on the stage sources first (the
  `ExternalArtifact`s and, where resolvable, their producer CRs), so an upstream
  re-publish happens before the StageSet re-reconciles. Best-effort; skips
  sources it cannot resolve/patch and reports them.
- `--update-now` — also set `stages.metio.wtf/update-now` to a fresh token,
  which the controller already honors to **apply a window-held rollout once**,
  bypassing `spec.updateWindows`. (Surfaced as a real feature, not reinvented.)
- `--wait` / `--timeout` — poll until `status.lastHandledReconcileAt` catches up
  to the written token (and, with `--update-now`, until `PendingUpdate` clears),
  or the timeout elapses.

If `spec.suspend` is set, the command **warns** that a suspended StageSet will
not act on the request (the controller intentionally does not stamp
`lastHandledReconcileAt` while suspended) and exits non-zero unless `--force` is
passed to acknowledge.

### Single stage (`--stage NAME`)

There is **no** per-stage reconcile mechanism in the controller today
(`StageStatus` has no reconcile-request field). This design adds a small,
well-scoped one:

- **API**: add `LastHandledReconcileAt string` to `StageStatus`
  (`api/v1/stageset_types.go`), regenerate deepcopy + CRD.
- **Annotation**: `stages.metio.wtf/reconcile-stage` whose value is
  `<stage-name>@<token>` (stage name + opaque token). Naming follows the
  project's `stages.metio.wtf/<thing>` convention.
- **Controller**: at reconcile, if the annotation names a stage and the token
  differs from that stage's `StageStatus.LastHandledReconcileAt`, **clear that
  stage's action ledger** (`ExecutedActions` / `LedgerRevision`) so its
  `pre`/`post` actions and stage-anchored migrations re-run this pass, then
  record the token in `StageStatus.LastHandledReconcileAt`. Manifests already
  re-apply every reconcile; the *novel* capability is forcing one stage's
  **side-effecting actions** (a smoke-test Job, an HTTP gate, a patch) to
  re-execute without touching the others' ledgers.

This is the genuinely new contribution — the Flux ecosystem has no
per-component force precedent — and it reuses the exact compare-token-to-status
idempotency pattern so the request fires exactly once.

The CLI's `reconcile --stage NAME` writes the annotation and (with `--wait`)
polls `status.stages[NAME].lastHandledReconcileAt`.

## `get` — human-readable status

```text
stagesetctl get [NAME] [-n NS] [-A]
```

Prints a table / detail view that `kubectl get stageset` cannot assemble:
Ready condition + reason, per-stage phase and applied revision, **pending
update** (held revision + next window open time from `status.pendingUpdate`),
deployed `version` and **pending migrations**, and last-applied revisions.
Read-only, needs only `get`/`list` on StageSets. `-o yaml/json` passthrough for
scripting; default is the human table. This is a convenience/observability
command — secondary to `diff`, but cheap and high-value for operators.

## Controller-side changes (kept minimal)

1. **`internal/apply` — add a dry-run `Diff`.** `apply.Applier` currently has no
   dry-run. Add:

   ```go
   // Diff server-side dry-run applies each object with the controller's field
   // manager and reports what would change, without persisting.
   func (a *Applier) Diff(ctx context.Context, objects []*unstructured.Unstructured) ([]DiffEntry, error)
   ```

   where `DiffEntry` carries the action and the `existing`/`merged`
   unstructured. Implemented over `ssa.ResourceManager.Diff`. This lives in the
   controller package (not the CLI) so it shares the field manager and is
   covered by the controller's envtest harness — and so the controller could
   later expose a dry-run mode itself.

2. **`StageStatus.LastHandledReconcileAt`** + the `reconcile-stage` annotation
   handling described above. New annotation constant alongside
   `updateNowAnnotation` in `internal/controller`. New per-stage status field +
   deepcopy + CRD regen + a `conditions`/reason no-op (no new Reason needed).

3. No other controller behavior changes. The whole-object reconcile and
   update-now paths already exist.

## Package layout

```text
cmd/stagesetctl/main.go            # 4-line shell → os.Exit(cli.Run(...))
internal/cli/
  cli.go            # root cobra cmd, ConfigFlags, IOStreams, Run() seam, exit-code mapping
  diff.go           # diff subcommand (wires preview + render)
  build.go          # build subcommand
  reconcile.go      # reconcile subcommand (whole + --stage)
  get.go            # get/status subcommand
  *_test.go         # per-command unit + envtest
internal/preview/   # the render→dry-run-diff→plan engine (client-taking, reusable, tested)
  preview.go        # ResolveAndBuild(stage) -> objects ; Diff(objects) -> entries ; PrunePlan(...)
  *_test.go
internal/diffrender/ # PURE rendering: unified diff, color, secret masking, noise strip, summary
  render.go
  mask.go
  *_test.go         # heavy table-driven + golden, zero cluster deps
internal/apply/     # + Diff() method (controller-shared)
api/v1/             # + StageStatus.LastHandledReconcileAt
internal/controller/ # + reconcile-stage annotation handling
```

`internal/diffrender` is deliberately dependency-light (unstructured +
stdlib + a diff lib) and carries the bulk of the unit tests — it is where
masking and exit-noise correctness is pinned, the way `internal/inventory`
isolates the diff *planning* logic. (It cannot be stdlib-only like
`inventory` — it needs `unstructured` and a diff lib — so it is not added to
the arch-go dependency-free rule; arch-go gains no new constraint, and the
existing `inventory` and `api` rules are unaffected.)

## Testing strategy (every layer)

1. **Pure unit (`internal/diffrender`)** — table-driven + golden:
   - unified-diff formatting for create/configure/delete/unchanged;
   - Secret masking (changed vs unchanged values, `stringData`, multiple
     distinct values → distinct `#N`, `--show-secrets` bypass);
   - noise stripping (managedFields/status/resourceVersion/etc. never appear);
   - summary-line counts; color on/off honoring `NO_COLOR`;
   - exit-code mapping (clean → 0, drift → 1, `--exit-code=false` → 0).
2. **Engine (`internal/preview`)** — against a controller-runtime **fake
   client** for resolve/plan, and against **envtest** for the SSA dry-run
   `Diff` (real apiserver merge/defaulting). Feed a known artifact tree
   (`--source-dir`) and assert the produced objects and the prune plan.
3. **`internal/apply.Diff`** — envtest: apply an object, then `Diff` a modified
   version, assert action + merged result; assert dry-run leaves the cluster
   untouched.
4. **Command integration (`internal/cli`)** — via the `Run(ctx, streams, args)`
   seam against envtest: create a `StageSet` + stub `ExternalArtifact` +
   `--source-dir`, run `diff`/`build`/`reconcile`/`get`, assert
   stdout/stderr/exit code, and (for reconcile) assert the annotation +
   `status.lastHandledReconcileAt` / per-stage token.
5. **Controller (`internal/controller`)** — envtest: the new `reconcile-stage`
   annotation clears the named stage's ledger and re-runs its actions exactly
   once per token; other stages' ledgers untouched; suspended is a no-op.
6. **Golden output** — representative end-to-end diff/build outputs under
   `testdata/golden/`, with a `-update` flag (JaaS convention), compared
   semantically where possible and byte-wise for the rendered diff text.
7. **kind smoke (`hack/smoke`)** — a `scenario-cli-*.sh` that, against a real
   cluster with a deployed StageSet: runs `diff` and asserts the summary/exit
   code, runs `reconcile` and asserts `lastHandledReconcileAt` advances, runs
   `reconcile --stage` and asserts the stage re-runs. Pure `kubectl` +
   `stagesetctl`, agnostic to how the controller was deployed (matches the
   existing two-angle smoke strategy).

## CI / lint / licensing

- All Go gates apply to the new packages: `go vet`, `staticcheck`
  (`checks=all`), `gofumpt`, `gosec`, `arch-go`, `govulncheck`, `-race` tests.
  `gosec` G-rules around the dry-run apply and any temp-file materialization
  get inline `// #nosec <rule> -- <invariant>` justifications, never config
  exclusions.
<!-- REUSE-IgnoreStart -->
- **REUSE**: every new `.go` file carries the 0BSD SPDX header
  (`// SPDX-FileCopyrightText: The stageset-controller Authors` /
  `// SPDX-License-Identifier: 0BSD`). `testdata/golden/**` is covered by a
  `REUSE.toml` glob (data files can't carry comments), matching the existing
  `config/**`/`docs/**` overrides.
<!-- REUSE-IgnoreEnd -->

- **go.mod**: `go mod tidy` promotes cobra/pflag/cli-runtime/go-difflib/jsondiff
  to direct; no new downloads.
- **arch-go**: no new rule; the `inventory` (stdlib-only) and `api`
  (no controller-runtime) constraints remain satisfied — the CLI lives in
  `internal/cli`/`internal/preview`/`internal/diffrender`, none of which
  `inventory` or `api` import.

## Implementation phases

1. **Scaffold + seam**: `cmd/stagesetctl`, `internal/cli` root with
   ConfigFlags/IOStreams/`Run` seam, exit-code mapping, `get` (read-only,
   simplest), with unit + envtest. Lands a working binary early.
2. **Render + `build`**: `internal/preview` resolve/fetch/build, `--source-dir`,
   `build` command, `internal/diffrender` masking + YAML render (build reuses
   masking). Unit + golden + offline tests.
3. **`diff`**: `apply.Diff` (controller-shared, envtest), prune plan via
   `inventory.ComputePlan`, unified-diff rendering, summary, exit codes,
   `--server-side`/`--as-tenant`. The headline phase; full test matrix.
4. **`reconcile`**: whole-object (`requestedAt`, `--with-source`,
   `--update-now`, `--wait`) + the controller-side single-stage mechanism
   (`StageStatus.LastHandledReconcileAt`, annotation, ledger clear) and
   `reconcile --stage`. Controller envtest + CLI envtest.
5. **Smoke + docs**: `hack/smoke/scenario-cli-*.sh`, README/flag docs, runbook
   cross-links. Verify the full CI-equivalent gate locally.

Each phase is committed only after its own tests **and** the full local CI gate
(`go vet`, `staticcheck`, `gofumpt -l .`, `gosec`, `arch-go`, `govulncheck`,
`-race` suite, REUSE) pass — auditing the phase before moving on.

## Future work (explicitly deferred)

- `apply`/`promote` interactive command (`tk apply`-style: diff → confirm →
  trigger). v1 stops at diff + reconcile; applying stays the controller's job.
- Multi-format diff (`dyff`/semantic) and a pluggable external differ
  (`*_EXTERNAL_DIFF` env, the `KUBECTL_EXTERNAL_DIFF` pattern).
- `diff` across a whole namespace / all StageSets at once.
- Release-pipeline packaging (multi-arch `stagesetctl` binaries, krew manifest).
- Automated port-forward to the storage service for unreachable artifact URLs.
