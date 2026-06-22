#!/usr/bin/env bash
# SPDX-FileCopyrightText: The stageset-controller Authors
# SPDX-License-Identifier: 0BSD
#
# Scenario: the stagesetctl CLI against a live StageSet. Plant an artifact and a
# StageSet, wait for it to apply, then drive the client commands the same way an
# operator would: `get` (status), `build` (render), `diff` (preview, clean then
# drifted), and `reconcile` (force). The in-cluster artifact URL is not routable
# from the runner, so build/diff render from the local source tree via
# --source-dir — the offline path the CLI is designed for.
#
# The binary is built from this repo's ./cmd/stagesetctl unless STAGESETCTL
# points at a prebuilt one. It uses the same kubeconfig as kubectl.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "${HERE}/../.." && pwd)"
# shellcheck source=lib.sh
. "${HERE}/lib.sh"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

STAGESETCTL="${STAGESETCTL:-}"
if [ -z "$STAGESETCTL" ]; then
  log "Build stagesetctl from ./cmd/stagesetctl"
  STAGESETCTL="${WORK}/stagesetctl"
  ( cd "$ROOT" && go build -o "$STAGESETCTL" ./cmd/stagesetctl )
fi
log "Using CLI: ${STAGESETCTL}"

log "Build the artifact tarball (a ConfigMap manifest) and serve it"
mkdir -p "${WORK}/src" "${WORK}/serve"
cat > "${WORK}/src/configmap.yaml" <<'EOF'
apiVersion: v1
kind: ConfigMap
metadata:
  name: cli-smoke-applied
  namespace: default
data:
  greeting: hello
EOF
DIGEST="$(make_tarball "${WORK}/src" "${WORK}/serve/artifact.tar.gz")"
serve_files default cli-artifact-server "${WORK}/serve"
plant_external_artifact default cli-smoke-artifact \
  "$(artifact_url default cli-artifact-server artifact.tar.gz)" "$DIGEST"

log "Create a StageSet consuming the artifact"
kubectl apply -f - <<'EOF'
apiVersion: stages.metio.wtf/v1
kind: StageSet
metadata:
  name: cli-smoke
  namespace: default
spec:
  interval: 1m
  stages:
    - name: app
      sourceRef:
        name: cli-smoke-artifact
EOF
wait_ready stageset cli-smoke default

# --- get ---
log "stagesetctl get — human-readable status"
# The per-stage rows are written a moment after Ready flips True (a separate
# status update), so poll the get output until the stage appears instead of
# racing the first status write — otherwise a fast run reads Ready=True but
# stage-less status. Bounded; the grep below still fails loudly if it never lands.
GET_OUT=""
for _ in $(seq 1 30); do
  GET_OUT="$("$STAGESETCTL" get cli-smoke -n default)"
  printf '%s' "$GET_OUT" | grep -q "app" && break
  sleep 2
done
printf '%s\n' "$GET_OUT"
printf '%s' "$GET_OUT" | grep -q "Name:.*cli-smoke" || die "get: missing name"
printf '%s' "$GET_OUT" | grep -q "app" || die "get: missing stage"

# --- build ---
log "stagesetctl build --source-dir — render the stage manifests"
BUILD_OUT="$("$STAGESETCTL" build cli-smoke -n default --source-dir "${WORK}/src")"
printf '%s\n' "$BUILD_OUT"
printf '%s' "$BUILD_OUT" | grep -q "kind: ConfigMap" || die "build: missing ConfigMap"
printf '%s' "$BUILD_OUT" | grep -q "cli-smoke-applied" || die "build: missing object name"

# --- diff (clean) ---
# The StageSet already applied this exact render, so a server-side dry-run diff
# must report no changes and exit 0 — the render-parity guarantee end to end.
log "stagesetctl diff — clean (no changes, exit 0)"
set +e
CLEAN_OUT="$("$STAGESETCTL" diff cli-smoke -n default --source-dir "${WORK}/src" --color never 2>&1)"
CLEAN_RC=$?
set -e
printf '%s\n' "$CLEAN_OUT"
[ "$CLEAN_RC" -eq 0 ] || die "clean diff should exit 0, got $CLEAN_RC"

# --- diff (drift) ---
log "stagesetctl diff — drifted source (configure, exit 1)"
mkdir -p "${WORK}/changed"
cat > "${WORK}/changed/configmap.yaml" <<'EOF'
apiVersion: v1
kind: ConfigMap
metadata:
  name: cli-smoke-applied
  namespace: default
data:
  greeting: goodbye
EOF
set +e
DRIFT_OUT="$("$STAGESETCTL" diff cli-smoke -n default --source-dir "${WORK}/changed" --color never 2>&1)"
DRIFT_RC=$?
set -e
printf '%s\n' "$DRIFT_OUT"
[ "$DRIFT_RC" -eq 1 ] || die "drifted diff should exit 1 (changes found), got $DRIFT_RC"
printf '%s' "$DRIFT_OUT" | grep -q "configure ConfigMap/cli-smoke-applied" || die "drift diff: missing configure line"
printf '%s' "$DRIFT_OUT" | grep -q "goodbye" || die "drift diff: missing changed value"

# --- reconcile ---
log "stagesetctl reconcile --wait — force and confirm the controller handled it"
"$STAGESETCTL" reconcile cli-smoke -n default --wait --timeout 90s
TOKEN="$(kubectl -n default get stageset cli-smoke \
  -o jsonpath='{.metadata.annotations.reconcile\.fluxcd\.io/requestedAt}')"
[ -n "$TOKEN" ] || die "reconcile: requestedAt annotation not set"

log "stagesetctl reconcile --stage app — single-stage force"
"$STAGESETCTL" reconcile cli-smoke -n default --stage app --wait --timeout 90s

log "Clean up"
kubectl -n default delete stageset cli-smoke --timeout=120s
log "scenario-cli PASSED"
