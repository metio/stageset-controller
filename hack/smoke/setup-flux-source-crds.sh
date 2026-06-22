#!/usr/bin/env bash
# SPDX-FileCopyrightText: The stageset-controller Authors
# SPDX-License-Identifier: 0BSD
#
# Installs Flux's REAL source CRDs (GitRepository, OCIRepository, Bucket, and
# ExternalArtifact) at a given Flux version, so the smoke exercises the
# controller against the actual, version-specific source-controller schema rather
# than a hand-written stub — this is what lets the smoke matrix sweep multiple
# Flux versions (parity with the jaas smoke). Only the CRDs are installed, not
# the controllers: the scenarios stamp status.artifact/status.conditions
# directly, so the contract under test (resolve -> fetch -> verify) needs the
# schema, not a running source-controller. ExternalArtifact ships in
# source-controller v1.7.0+ (Flux v2.7.0+), which is the floor the discover job
# enforces. Usage: setup-flux-source-crds.sh [flux-version]
set -euo pipefail
VERSION="${1:-v2.7.0}"

install_yaml="$(mktemp)"
crds_yaml="$(mktemp)"
trap 'rm -f "$install_yaml" "$crds_yaml"' EXIT

# Flux publishes one combined install.yaml per release; we extract only its
# source.toolkit.fluxcd.io CRDs.
curl -fsSL --retry 5 --retry-all-errors \
  "https://github.com/fluxcd/flux2/releases/download/${VERSION}/install.yaml" -o "$install_yaml"

# install.yaml is a well-formed multi-document stream separated by '\n---\n';
# keep the documents that are CRDs in the source.toolkit group. Plain string
# split (no PyYAML dependency) is enough for Flux's generated manifest.
python3 - "$install_yaml" "$crds_yaml" <<'PY'
import sys
docs = open(sys.argv[1]).read().split('\n---\n')
keep = [d for d in docs
        if 'kind: CustomResourceDefinition' in d
        and 'group: source.toolkit.fluxcd.io' in d]
open(sys.argv[2], 'w').write('\n---\n'.join(keep))
PY
[ -s "$crds_yaml" ] || { echo "no source.toolkit CRDs found in Flux ${VERSION} install.yaml" >&2; exit 1; }

# Server-side apply: the real source CRDs are large and would blow the
# client-side last-applied-configuration annotation size limit.
kubectl apply --server-side -f "$crds_yaml"
for plural in gitrepositories ocirepositories buckets externalartifacts; do
  kubectl wait --for=condition=Established \
    "crd/${plural}.source.toolkit.fluxcd.io" --timeout=60s
done
