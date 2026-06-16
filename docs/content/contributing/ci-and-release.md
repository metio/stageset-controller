---
title: CI and releases
description: The pull-request gates and the automated calendar-based release process.
tags: [contributing, ci, release]
---

## Continuous integration

Every pull request runs `verify.yml`, which fans out into one job per concern so a
failure points straight at the cause:

- **test** — `go build` then the full `go test` suite.
- **lint-go** — `go vet`, `staticcheck`, `gosec`, and a `gofumpt` formatting check.
- **vulnerabilities** — `govulncheck` (a reachable advisory is a hard gate).
- **architecture** — `arch-go` against `arch-go.yml`.
- **reuse** — SPDX/REUSE compliance on every file.
- **text linters** — `yamllint`, `actionlint`, `markdownlint`, `typos`.
- **container-image** — a buildx image build plus a Trivy scan.

A single **all-green** job depends on every other job and is the only required
check, so new jobs are covered automatically. A separate `kind-smoke.yml` runs the
operator end to end against a real kind cluster, and `fuzz.yml` exercises the fuzz
targets.

## Releases

Releases are **calendar-based and fully automated** — there is no semver tag to
bump by hand. `release.yml` runs on a Monday cron (and on manual dispatch), and the
version is the run date (`date +'%Y.%-m.%-d'`, e.g. `2026.6.15`). A prepare job
counts commits since the last release; an empty week publishes nothing.

The pipeline is hand-rolled — no goreleaser, no GPG:

- Binaries are cross-compiled with `go build` (`CGO_ENABLED=0`, `-trimpath`,
  `-ldflags`) and archived per platform.
- A multi-arch image is pushed to `ghcr.io/metio/stageset-controller` and signed
  with **cosign keyless** (Fulcio OIDC) by digest.
- The GitHub Release attaches the archives, a `SHA256SUMS` file, and its cosign
  signature; identity is proven by the workflow's OIDC certificate, so there is no
  key to distribute.

The Helm chart lives in the [metio/helm-charts](https://github.com/metio/helm-charts)
repository and vendors this repo's CRDs at each release; its `appVersion` tracks the
binary's releases.
