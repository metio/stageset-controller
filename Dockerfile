# SPDX-FileCopyrightText: The stageset-controller Authors
# SPDX-License-Identifier: 0BSD

# The builder is pinned to the native build platform so multi-arch image builds
# cross-compile through Go's GOOS/GOARCH (CGO is disabled) instead of emulating
# each target under QEMU. buildx supplies TARGETOS/TARGETARCH/TARGETVARIANT per
# target; the runtime stage below takes $TARGETPLATFORM and pulls the matching
# distroless base.
FROM --platform=$BUILDPLATFORM cgr.dev/chainguard/go@sha256:3cea88773e65f24c4db570d96b97a65fb8f3c145f656a4396e23d9be6f34cddd AS build
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=development
ARG COMMIT=development
ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} GOARM=${TARGETVARIANT#v} \
    go build -trimpath \
    -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
    -o stageset-controller ./cmd

# distroless/static publishes amd64, arm64, arm/v7, ppc64le, riscv64, and s390x
# — the same arch set every metio image ships, so this controller is
# co-schedulable with jaas and the JOI images on any node architecture.
FROM gcr.io/distroless/static:nonroot@sha256:963fa6c544fe5ce420f1f54fb88b6fb01479f054c8056d0f74cc2c6000df5240
COPY --from=build /app/stageset-controller /usr/bin/
ENTRYPOINT ["/usr/bin/stageset-controller"]
