#!/usr/bin/env bash
# SPDX-FileCopyrightText: The stageset-controller Authors
# SPDX-License-Identifier: 0BSD
#
# Scenario: a stage can consume a classic Flux source — GitRepository,
# OCIRepository, or Bucket — directly, not only an ExternalArtifact. For each
# kind, plant a Ready source whose artifact renders a ConfigMap, point a StageSet
# at it, and verify the pipeline runs and the manifest is applied. The StageSets
# omit spec.interval, so this also exercises the --default-interval fallback.
#
# Requires the stub source CRDs from setup-flux-source-crds.sh.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
. "${HERE}/lib.sh"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

for entry in "GitRepository:git" "OCIRepository:oci" "Bucket:bucket"; do
  kind="${entry%%:*}"
  slug="${entry##*:}"
  cm="direct-${slug}"
  log "Direct source: ${kind}"

  mkdir -p "${WORK}/${slug}/src" "${WORK}/${slug}/serve"
  cat > "${WORK}/${slug}/src/configmap.yaml" <<EOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: ${cm}
  namespace: default
data:
  from: ${kind}
EOF
  digest="$(make_tarball "${WORK}/${slug}/src" "${WORK}/${slug}/serve/artifact.tar.gz")"
  serve_files default "src-${slug}" "${WORK}/${slug}/serve"
  plant_flux_source "$kind" default "repo-${slug}" \
    "$(artifact_url default "src-${slug}" artifact.tar.gz)" "$digest"

  # No spec.interval — exercises the --default-interval fallback.
  kubectl apply -f - <<EOF
apiVersion: stages.metio.wtf/v1
kind: StageSet
metadata:
  name: direct-${slug}
  namespace: default
spec:
  stages:
    - name: app
      sourceRef:
        kind: ${kind}
        name: repo-${slug}
EOF

  wait_ready stageset "direct-${slug}" default
  test "$(kubectl -n default get configmap "${cm}" -o jsonpath='{.data.from}')" = "${kind}" \
    || die "${kind}: applied ConfigMap content mismatch"
  log "${kind} direct source PASSED"
done

log "scenario-direct-source PASSED"
