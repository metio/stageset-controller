#!/usr/bin/env bash
# SPDX-FileCopyrightText: The stageset-controller Authors
# SPDX-License-Identifier: 0BSD
#
# Reconciler throughput at scale: apply N StageSets at once, all consuming one
# shared ExternalArtifact but each rendering a distinct ConfigMap via its own
# postBuild substitution, and assert every one converges to Ready=True inside a
# generous window. Catches obvious reconcile-throughput regressions under
# fan-out — workqueue saturation, leader-election thrashing, GC pressure from
# many parallel builds/applies, or a stuck/starved reconcile loop. Env: NS, N.
# Assumes the controller is deployed and the ExternalArtifact stub CRD installed.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
. "${HERE}/lib.sh"

NS="${NS:-default}"
N="${N:-50}"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

log "Build one shared artifact whose ConfigMap name is templated on \${IDX}"
# A single artifact serves all N StageSets; each StageSet picks a unique IDX
# through postBuild.substitute, so the N applies never collide on one object —
# every StageSet owns its own scale-applied-<idx> ConfigMap.
mkdir -p "${WORK}/src" "${WORK}/serve"
cat > "${WORK}/src/configmap.yaml" <<'EOF'
apiVersion: v1
kind: ConfigMap
metadata:
  name: scale-applied-${IDX}
  namespace: default
data:
  # postBuild substitution is textual and kustomize drops the quotes around a
  # plain scalar, so a bare numeric value would land as a YAML int and fail the
  # ConfigMap string-map apply. A non-numeric value stays a string unquoted.
  idx: "stage-${IDX}"
EOF
DIGEST="$(make_tarball "${WORK}/src" "${WORK}/serve/artifact.tar.gz")"

log "Serve it in-cluster and plant the shared ExternalArtifact"
serve_files "$NS" scale-server "${WORK}/serve"
plant_external_artifact "$NS" scale-artifact \
  "$(artifact_url "$NS" scale-server artifact.tar.gz)" "$DIGEST"

log "Apply $N StageSets at once, each with its own postBuild substitution"
for i in $(seq 1 "$N"); do
  cat <<EOF
---
apiVersion: stages.metio.wtf/v1
kind: StageSet
metadata:
  name: scale-${i}
  namespace: ${NS}
spec:
  interval: 1m
  stages:
    - name: app
      sourceRef:
        name: scale-artifact
      postBuild:
        substitute:
          IDX: "${i}"
EOF
done | kubectl apply -f -

log "Wait for every StageSet to reach Ready=True"
# N StageSets serialised through the workqueue converge well under a minute on
# kind; allow generous headroom so a slow runner is not a false negative while a
# genuinely stuck reconcile still trips the deadline.
deadline=$(( $(date +%s) + 300 ))
while [ "$(date +%s)" -lt "$deadline" ]; do
  total=$(kubectl -n "$NS" get stageset -o name | wc -l)
  ready=$(kubectl -n "$NS" get stageset \
    -o jsonpath='{range .items[*]}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}' \
    | grep -c True || true)
  log "ready ${ready}/${total}"
  if [ "$ready" = "$total" ] && [ "$total" = "$N" ]; then
    log "all $N StageSets converged to Ready=True"
    break
  fi
  sleep 5
  if [ "$(date +%s)" -ge "$deadline" ]; then
    log "convergence timeout — per-StageSet Ready status (non-True rows are the laggards):"
    # kubectl's jsonpath cannot nest filters, so list every StageSet through a
    # single Ready-condition filter; the non-True rows stand out and their
    # reason+message name why they are stuck.
    kubectl -n "$NS" get stageset -o custom-columns=\
'NAME:.metadata.name,READY:.status.conditions[?(@.type=="Ready")].status,REASON:.status.conditions[?(@.type=="Ready")].reason,MESSAGE:.status.conditions[?(@.type=="Ready")].message' || true
    die "not all StageSets converged within the window"
  fi
done

log "Verify a sample of the rendered ConfigMaps were applied with the right IDX"
# Spot-check the first, a middle, and the last StageSet's applied object — each
# proves its own postBuild substitution ran and its manifest landed, so the N
# converged Ready conditions are backed by N real applies, not empty renders.
for i in 1 $(( (N + 1) / 2 )) "$N"; do
  got="$(kubectl -n "$NS" get configmap "scale-applied-${i}" -o jsonpath='{.data.idx}' 2>/dev/null || true)"
  [ "$got" = "stage-${i}" ] || die "scale-applied-${i} missing or wrong (.data.idx=${got:-<none>}, want stage-${i})"
done

log "Tear down — delete every scale StageSet; finalizers must prune the applied objects"
names=""
for i in $(seq 1 "$N"); do names="${names} scale-${i}"; done
# shellcheck disable=SC2086
kubectl -n "$NS" delete stageset ${names} --timeout=180s
remaining="$(kubectl -n "$NS" get configmap -o name 2>/dev/null | grep -c '/scale-applied-' || true)"
[ "$remaining" = "0" ] || die "finalizer teardown left ${remaining} scale-applied-* ConfigMaps behind"

log "Clean up the shared artifact + file server"
kubectl -n "$NS" delete externalartifact scale-artifact --ignore-not-found
kubectl -n "$NS" delete deployment,service scale-server --ignore-not-found
kubectl -n "$NS" delete configmap scale-server-data --ignore-not-found

log "scenario-scale PASSED"
