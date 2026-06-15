#!/usr/bin/env bash
# SPDX-FileCopyrightText: The stageset-controller Authors
# SPDX-License-Identifier: 0BSD
#
# Scenario: the happy path. Plant an ExternalArtifact whose tarball renders a
# ConfigMap, point a StageSet at it, and verify the full pipeline runs — Ready,
# the manifest applied, a StageInventory recorded — then delete the StageSet and
# verify the finalizer prunes the applied object.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
. "${HERE}/lib.sh"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

log "Build the artifact tarball (a ConfigMap manifest)"
mkdir -p "${WORK}/src" "${WORK}/serve"
cat > "${WORK}/src/configmap.yaml" <<'EOF'
apiVersion: v1
kind: ConfigMap
metadata:
  name: smoke-applied
  namespace: default
data:
  from: stageset-controller-smoke
EOF
DIGEST="$(make_tarball "${WORK}/src" "${WORK}/serve/artifact.tar.gz")"

log "Serve it in-cluster and plant the ExternalArtifact"
serve_files default artifact-server "${WORK}/serve"
plant_external_artifact default smoke-artifact \
  "$(artifact_url default artifact-server artifact.tar.gz)" "$DIGEST"

log "Create a StageSet consuming the artifact"
kubectl apply -f - <<'EOF'
apiVersion: stages.metio.wtf/v1
kind: StageSet
metadata:
  name: smoke
  namespace: default
spec:
  interval: 1m
  stages:
    - name: app
      sourceRef:
        name: smoke-artifact
EOF

wait_ready stageset smoke default

log "Verify the rendered manifest was applied"
test "$(kubectl -n default get configmap smoke-applied -o jsonpath='{.data.from}')" \
  = "stageset-controller-smoke" || die "applied ConfigMap content mismatch"

log "Verify a StageInventory was recorded"
kubectl -n default get stageinventories -o wide
[ "$(kubectl -n default get stageinventories \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' | grep -c .)" -ge 1 ] \
  || die "no StageInventory recorded"

log "Delete the StageSet — the finalizer must prune the applied object"
kubectl -n default delete stageset smoke --timeout=120s
if kubectl -n default get configmap smoke-applied 2>/dev/null; then
  die "finalizer teardown left the applied ConfigMap behind"
fi
log "scenario-basic PASSED"
