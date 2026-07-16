---
title: Building and testing
description: Build the controller, run the test suite, and pass the static-analysis gate.
tags: [contributing, ci]
---

The host needs no Go toolchain. Every command runs through the development shell
that `flake.nix` defines, with `flake.lock` pinning each tool to an exact
version:

```shell
nix develop --command go build ./...
nix develop --command go test -race -cover ./...
```

CI runs the same shell, so a gate that is green locally is green there by
construction. Run `nix develop` on its own to drop into an interactive shell and
call the tools bare.

## Test layers

- **Unit tests** sit next to the code across `internal/...` and `api/v1/`. Several
  are drift gates — e.g. `conditions_test.go` asserts every Ready `Reason` has a
  matching runbook page under `docs/content/runbooks/`.
- **envtest-backed tests** (`envtest_*_test.go`) boot a real kube-apiserver + etcd
  via controller-runtime's `envtest`. The shell exports `KUBEBUILDER_ASSETS`
  pointing at an `etcd` + `kube-apiserver` + `kubectl` bundle assembled from
  nixpkgs, so they run offline with nothing to install; they `t.Skip` when it is
  unset.
- **Fuzz tests** (`FuzzXxx`) harden the parsing-heavy paths; their seed corpus runs
  as ordinary unit tests, and `-fuzz` fuzzes for real:

  ```shell
  nix develop --command go test -run=^$ -fuzz=^FuzzName$ -fuzztime=30s ./internal/<pkg>/
  ```

- **Kind smoke** scenarios under `hack/smoke/` run the controller end to end
  against a real kind cluster.

## Regenerating generated code

The CRDs under `config/crd/`, `config/rbac/role.yaml` (rendered from the
`+kubebuilder:rbac` markers), `config/webhook/manifests.yaml`, and
`api/v1/zz_generated.deepcopy.go` are produced by `controller-gen`. Regenerate
them after touching `api/` or a marker, and commit the result:

```shell
nix develop --command generate
```

`verify.yml`'s `generated` job runs that same command and fails on any diff, so
stale manifests cannot ship.

## Building the site

```shell
nix develop --command website   # one-shot build into docs/public/
nix develop --command serve     # live server on :1313
nix develop --command htmltest  # lint the rendered HTML (needs a build first)
```

## Static analysis

A pull request must be clean under each of these — run them locally before
pushing:

```shell
nix develop --command go vet ./...
nix develop --command staticcheck ./...      # config: staticcheck.conf, checks = ["all"]
nix develop --command gosec ./...
nix develop --command govulncheck ./...
nix develop --command gofumpt -l .           # empty output == formatted
nix develop --command arch-go                # architecture rules (arch-go.yml)
nix develop --command modernize ./...        # newer-Go idiom check
```
