---
title: Building and testing
description: Build the controller, run the test suite, and pass the static-analysis gate.
tags: [contributing, ci]
---

The controller is a standard Go module. With a Go toolchain installed:

```shell
go build ./...
go test -race -cover ./...
```

## Test layers

- **Unit tests** sit next to the code across `internal/...` and `api/v1/`. Several
  are drift gates — e.g. `conditions_test.go` asserts every Ready `Reason` has a
  matching runbook page under `docs/content/runbooks/`.
- **envtest-backed tests** (`envtest_*_test.go`) boot a real kube-apiserver + etcd
  via controller-runtime's `envtest`. They `t.Skip` unless `KUBEBUILDER_ASSETS`
  points at an asset bundle — install it with
  [`setup-envtest`](https://book.kubebuilder.io/reference/envtest.html).
- **Fuzz tests** (`FuzzXxx`) harden the parsing-heavy paths; their seed corpus runs
  as ordinary unit tests, and `-fuzz` fuzzes for real.
- **Kind smoke** scenarios under `hack/smoke/` run the controller end to end
  against a real kind cluster.

## Static analysis

A pull request must be clean under each of these — run them locally before
pushing:

```shell
go vet ./...
go run honnef.co/go/tools/cmd/staticcheck@latest ./...
go run github.com/securego/gosec/v2/cmd/gosec@latest ./...
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
go run mvdan.cc/gofumpt@latest -l .          # empty output == formatted
go run github.com/fe3dback/arch-go@latest    # architecture rules (arch-go.yml)
```

## Containerized dev shell

The toolchain — including the pinned `setup-envtest` assets — is also packaged in
a container via [ilo](https://ilo.projects.metio.wtf/), so you can build and test
without installing anything on the host:

```shell
ilo bash -c 'go test -race -cover ./...'
```
