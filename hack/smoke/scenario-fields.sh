#!/usr/bin/env bash
# SPDX-FileCopyrightText: The stageset-controller Authors
# SPDX-License-Identifier: 0BSD
#
# Scenario: newer spec fields + Kubernetes Events. Drives spec.suspend
# (pause/resume reconciliation) and asserts the controller emits an events.v1
# Event on the Ready=Succeeded transition — the signal notification-controller
# routes on. A suspended StageSet must hold Ready=False/Suspended and not
# re-apply; resuming must drive it back to Ready=True. Assumes the controller is
# deployed and the ExternalArtifact CRD is installed (webhook not required).
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
. "${HERE}/lib.sh"

NS="${NS:-default}"
NAME="${NAME:-fields-demo}"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"; kubectl -n "$NS" delete stageset "$NAME" --ignore-not-found --timeout=120s >/dev/null 2>&1 || true' EXIT

# has_event <name> <ns> <type> <reason> — true if a matching Event exists on the
# named object. The controller fills both the Event reason and action slots with
# the same string, so matching on reason is sufficient.
has_event() {
  kubectl -n "$2" get events --field-selector "involvedObject.name=$1" \
    -o jsonpath="{.items[?(@.type==\"$3\")].reason}" 2>/dev/null | grep -qw "$4"
}

log "Build the artifact tarball (a ConfigMap manifest)"
mkdir -p "${WORK}/src" "${WORK}/serve"
cat > "${WORK}/src/configmap.yaml" <<EOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: ${NAME}-applied
  namespace: ${NS}
data:
  from: stageset-fields-smoke
EOF
DIGEST="$(make_tarball "${WORK}/src" "${WORK}/serve/artifact.tar.gz")"

log "Serve it in-cluster and plant the ExternalArtifact"
serve_files "$NS" "${NAME}-server" "${WORK}/serve"
plant_external_artifact "$NS" "${NAME}-artifact" \
  "$(artifact_url "$NS" "${NAME}-server" artifact.tar.gz)" "$DIGEST"

log "Create a StageSet consuming the artifact"
kubectl apply -f - <<EOF
apiVersion: stages.metio.wtf/v1
kind: StageSet
metadata:
  name: ${NAME}
  namespace: ${NS}
spec:
  interval: 1m
  stages:
    - name: app
      sourceRef:
        name: ${NAME}-artifact
EOF

wait_ready stageset "$NAME" "$NS"

log "Verify the rendered manifest was applied"
test "$(kubectl -n "$NS" get configmap "${NAME}-applied" -o jsonpath='{.data.from}')" \
  = "stageset-fields-smoke" || die "applied ConfigMap content mismatch"

log "Verify a Normal/Succeeded Event was emitted on the Ready transition"
for i in $(seq 1 30); do
  has_event "$NAME" "$NS" Normal Succeeded && { log "Normal/Succeeded Event seen after $i polls"; break; }
  sleep 2
done
has_event "$NAME" "$NS" Normal Succeeded || {
  kubectl -n "$NS" get events --field-selector "involvedObject.name=$NAME" >&2 || true
  die "no Normal/Succeeded Event recorded"
}

log "Suspend the StageSet — reconciliation must pause (Ready=False/Suspended)"
kubectl -n "$NS" patch stageset "$NAME" --type=merge -p '{"spec":{"suspend":true}}'
wait_reason stageset "$NAME" "$NS" Suspended
[ "$(ready_status stageset "$NAME" "$NS")" = "False" ] \
  || die "suspended StageSet did not report Ready=False"

log "While suspended, deleting the applied object must NOT be re-applied"
kubectl -n "$NS" delete configmap "${NAME}-applied"
# A suspended StageSet short-circuits before apply; give the steady requeue a
# window to (wrongly) re-create the ConfigMap, then assert it stayed gone.
for i in $(seq 1 10); do
  sleep 3
  if kubectl -n "$NS" get configmap "${NAME}-applied" >/dev/null 2>&1; then
    die "suspended StageSet re-applied the manifest (reconciliation was not paused)"
  fi
done
log "ConfigMap stayed absent while suspended ($i checks)"

log "Resume the StageSet — reconciliation must re-run and re-apply"
kubectl -n "$NS" patch stageset "$NAME" --type=merge -p '{"spec":{"suspend":false}}'
wait_ready stageset "$NAME" "$NS"
test "$(kubectl -n "$NS" get configmap "${NAME}-applied" -o jsonpath='{.data.from}')" \
  = "stageset-fields-smoke" || die "resume did not re-apply the manifest"

log "scenario-fields PASSED"
