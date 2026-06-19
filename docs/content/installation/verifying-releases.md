---
title: Verifying releases
description: Verify the signed container image, release archives, and stagesetctl CLI before you run them.
tags: [installation, supply-chain, cosign, sigstore, signing, verification]
---

Every release is signed with [cosign](https://github.com/sigstore/cosign) keyless
signing. There is no GPG key to import and no public key to distribute — the
signature's trust comes from the GitHub Actions workflow's OIDC identity, proven by
a [Fulcio](https://docs.sigstore.dev/) certificate and logged in
[Rekor](https://docs.sigstore.dev/logging/overview/). Verify the artifacts you pull
before you run them.

Install cosign from [its releases](https://github.com/sigstore/cosign/releases), or
on most systems:

```shell
go install github.com/sigstore/cosign/v2/cmd/cosign@latest
```

Two values appear in every command below — the workflow identity and the OIDC
issuer. They are the same for the image and the blobs:

- `--certificate-identity-regexp '^https://github.com/metio/stageset-controller/\.github/workflows/release\.yml@refs/'`
- `--certificate-oidc-issuer https://token.actions.githubusercontent.com`

## Verify the container image

Verify by digest for an exact, immutable match. Resolve the digest first, then
verify it:

```shell
VERSION=2026.6.15   # the release you are installing
DIGEST=$(cosign verify ghcr.io/metio/stageset-controller:"${VERSION}" \
  --certificate-identity-regexp '^https://github.com/metio/stageset-controller/\.github/workflows/release\.yml@refs/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --output json | jq -r '.[0].critical.image."docker-manifest-digest"')

cosign verify ghcr.io/metio/stageset-controller@"${DIGEST}" \
  --certificate-identity-regexp '^https://github.com/metio/stageset-controller/\.github/workflows/release\.yml@refs/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

A successful verification prints the certificate's subject and issuer and confirms
the signature is recorded in the transparency log. A failure exits non-zero — do not
run an image that fails to verify.

## Inspect the SBOM and provenance

Each image ships an SBOM and SLSA provenance attestation, attached at build time.
Read them with cosign:

```shell
# Software bill of materials
cosign download sbom ghcr.io/metio/stageset-controller@"${DIGEST}"

# Build provenance (who built it, from which commit, in which workflow)
cosign verify-attestation ghcr.io/metio/stageset-controller@"${DIGEST}" \
  --type slsaprovenance \
  --certificate-identity-regexp '^https://github.com/metio/stageset-controller/\.github/workflows/release\.yml@refs/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

## Verify the release archives and CLI

The GitHub release attaches one archive per platform for both binaries — the
`stageset-controller` daemon and the `stagesetctl` CLI — plus a single
`stageset-controller_<version>_SHA256SUMS` file covering every archive and a cosign
bundle signing that checksum file. Verify the checksum file's signature, then check
your downloads against it.

Download the archive(s) you need, the `SHA256SUMS` file, and its `.bundle` from the
[release page](https://github.com/metio/stageset-controller/releases), then:

```shell
VERSION=2026.6.15

# 1. Verify the checksum file's signature. The newer cosign bundle format carries
#    the signature, certificate, and Rekor proof in one file.
cosign verify-blob stageset-controller_"${VERSION}"_SHA256SUMS \
  --bundle stageset-controller_"${VERSION}"_SHA256SUMS.bundle \
  --certificate-identity-regexp '^https://github.com/metio/stageset-controller/\.github/workflows/release\.yml@refs/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com

# 2. Check your downloaded archives against the verified checksums.
sha256sum -c stageset-controller_"${VERSION}"_SHA256SUMS
```

`sha256sum -c` reports `OK` for every archive listed in the file that is present in
the current directory and skips the rest, so you only need to download the platforms
you use. Once an archive verifies, unpack it and run the binary:

```shell
tar xf stagesetctl_"${VERSION}"_linux_amd64.tar.gz
./stagesetctl_v"${VERSION}" version
```

The CLI doubles as a `kubectl` plugin: rename it to `kubectl-stageset` on your
`PATH` and call it as `kubectl stageset …`.
