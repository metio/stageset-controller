#!/usr/bin/env bash
# SPDX-FileCopyrightText: The stageset-controller Authors
# SPDX-License-Identifier: 0BSD
#
# High-availability path: with replicas.min/max=2 the controller runs two pods
# but only the Lease holder reconciles (leader election). Killing the leader must
# hand the Lease to the surviving replica (LeaderElectionReleaseOnCancel makes
# this fast), and a StageSet applied after the handover must still go Ready —
# proving the new leader took over reconciliation. Env: NS, NAME, LEASE (the
# Lease name; the controller hardcodes it to
# "stageset-controller.stages.metio.wtf"), CNS (controller install namespace).
# Assumes the controller is deployed with replicas.min/max >= 2 and the
# ExternalArtifact stub CRD is installed.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
. "${HERE}/lib.sh"

NS="${NS:-default}"
NAME="${NAME:-ha-demo}"
CNS="${CNS:-stageset-system}"
LEASE="${LEASE:-stageset-controller.stages.metio.wtf}"
CM="${NAME}-applied"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

log "Wait for 2 controller pods to be Ready"
ready=0
for i in $(seq 1 60); do
  ready=$(kubectl -n "$CNS" get pods -l app.kubernetes.io/name=stageset-controller \
    -o jsonpath='{range .items[*]}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}' \
    | grep -c True || true)
  [ "$ready" -ge 2 ] && { log "$ready controller pods Ready after $i polls"; break; }
  sleep 5
done
[ "$ready" -ge 2 ] || {
  kubectl -n "$CNS" get pods -l app.kubernetes.io/name=stageset-controller -o wide
  die "fewer than 2 controller pods became Ready"
}

# lease_holder — echoes the current Lease holderIdentity (or "").
lease_holder() {
  kubectl -n "$CNS" get lease "$LEASE" -o jsonpath='{.spec.holderIdentity}' 2>/dev/null || true
}

log "Wait for a leader to acquire the Lease"
HOLDER=""
for i in $(seq 1 60); do
  HOLDER="$(lease_holder)"
  [ -n "$HOLDER" ] && { log "Lease $LEASE held by $HOLDER after $i polls"; break; }
  sleep 2
done
[ -n "$HOLDER" ] || { kubectl -n "$CNS" get lease; die "no leader acquired the Lease"; }

# The holderIdentity is "<pod-name>_<uuid>"; the pod name is the prefix.
LEADER_POD="${HOLDER%%_*}"
log "Current leader pod: $LEADER_POD"
kubectl -n "$CNS" get pod "$LEADER_POD" >/dev/null 2>&1 || die "leader pod $LEADER_POD not found"

log "Delete the leader pod to force a handover"
kubectl -n "$CNS" delete pod "$LEADER_POD" --wait=false

log "Wait for a NEW holder to be elected"
NEW=""
for i in $(seq 1 60); do
  NEW="$(lease_holder)"
  if [ -n "$NEW" ] && [ "$NEW" != "$HOLDER" ]; then
    log "Lease handed over to $NEW after $i polls"; break
  fi
  sleep 2
done
[ -n "$NEW" ] && [ "$NEW" != "$HOLDER" ] || {
  kubectl -n "$CNS" get lease "$LEASE" -o yaml
  die "Lease was not handed to a new holder"
}

log "Apply a StageSet after the handover — the new leader must reconcile it"
mkdir -p "${WORK}/src" "${WORK}/serve"
cat > "${WORK}/src/configmap.yaml" <<EOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: ${CM}
  namespace: ${NS}
data:
  from: ha-after-failover
EOF
DIGEST="$(make_tarball "${WORK}/src" "${WORK}/serve/artifact.tar.gz")"
serve_files "$NS" "${NAME}-server" "${WORK}/serve"
plant_external_artifact "$NS" "${NAME}-artifact" \
  "$(artifact_url "$NS" "${NAME}-server" artifact.tar.gz)" "$DIGEST"

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

wait_ready stageset "$NAME" "$NS" 90 5
test "$(kubectl -n "$NS" get configmap "$CM" -o jsonpath='{.data.from}')" \
  = "ha-after-failover" || die "StageSet reconciled but the manifest was not applied"

log "StageSet went Ready and applied after leader failover — HA verified"
log "scenario-ha PASSED"
