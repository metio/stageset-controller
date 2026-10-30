---
title: Building and testing
description: Build the controller, run the test suite, and pass the static-analysis gate.
tags: [contributing, ci]
---

The host needs no Go toolchain. `.breto.edn` names the
[metio/toolchains](https://github.com/metio/toolchains) groups that pin each tool
to an exact version, and [breto](https://github.com/metio/breto) installs them
and puts them first on `PATH` in the project's shell:

```shell
eval "$(breto env)"
go build ./...
go test -race -cover ./...
```

CI installs the same versions with the breto action, so a gate that is green
locally is green there as well. breto's prompt hook does the same on entering
the project.

## Test layers

- **Unit tests** sit next to the code across `internal/...` and `api/v1/`. Several
  are drift gates — e.g. `conditions_test.go` asserts every Ready `Reason` has a
  matching runbook page under `docs/content/runbooks/`.
- **envtest-backed tests** (`envtest_*_test.go`) boot a real kube-apiserver + etcd
  via controller-runtime's `envtest`. The shell exports `KUBEBUILDER_ASSETS`
  pointing at the `etcd` + `kube-apiserver` + `kubectl` bundle of controller-tools'
  envtest releases, so they run offline once it is installed; they `t.Skip` when
  it is unset.
- **Fuzz tests** (`FuzzXxx`) harden the parsing-heavy paths; their seed corpus runs
  as ordinary unit tests, and `-fuzz` fuzzes for real:

  ```shell
  go test -run=^$ -fuzz=^FuzzName$ -fuzztime=30s ./internal/<pkg>/
  ```

- **Kind smoke** scenarios under `hack/smoke/` run the controller end to end
  against a real kind cluster.

## Regenerating generated code

The CRDs under `config/crd/`, `config/rbac/role.yaml` (rendered from the
`+kubebuilder:rbac` markers), `config/webhook/manifests.yaml`, and
`api/v1/zz_generated.deepcopy.go` are produced by `controller-gen`. Regenerate
them after touching `api/` or a marker, and commit the result:

```shell
generate
```

`verify.yml`'s `generated` job runs that same command and fails on any diff, so
stale manifests cannot ship.

## Building the site

```shell
website   # one-shot build into docs/public/
serve     # live server on :1313
htmltest  # lint the rendered HTML (needs a build first)
```

## Static analysis

A pull request must be clean under each of these — run them locally before
pushing:

```shell
go vet ./...
staticcheck ./...      # config: staticcheck.conf, checks = ["all"]
gosec ./...
govulncheck ./...
gofumpt -l .           # empty output == formatted
arch-go                # architecture rules (arch-go.yml)
modernize ./...        # newer-Go idiom check
```
