#!/usr/bin/env bash
# SPDX-FileCopyrightText: The stageset-controller Authors
# SPDX-License-Identifier: 0BSD
#
# Scenario: cross-namespace artifact fetch under enforced NetworkPolicies. The
# controller fetches an ExternalArtifact's tarball over HTTP from the producer's
# storage server, which commonly lives in another namespace. If that namespace
# default-denies ingress (as Flux's flux-system does), the fetch is dropped and
# the StageSet stalls on StageFailed until an allow rule names the controller's
# namespace. This pins that contract: deny blocks the fetch, an allow rule for
# the controller's namespace unblocks it.
#
# CONTROLLER_NS (default stageset-system) is the namespace the controller pod
# runs in — the allow rule is scoped to it, mirroring the production guidance.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
. "${HERE}/lib.sh"

CONTROLLER_NS="${CONTROLLER_NS:-stageset-system}"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

log "Serve an artifact in a separate, locked-down namespace (np-test)"
kubectl create namespace np-test
mkdir -p "${WORK}/src" "${WORK}/serve"
cat > "${WORK}/src/configmap.yaml" <<'EOF'
apiVersion: v1
kind: ConfigMap
metadata:
  name: np-applied
  namespace: np-test
data:
  from: reached-through-allow-rule
EOF
DIGEST="$(make_tarball "${WORK}/src" "${WORK}/serve/artifact.tar.gz")"
serve_files np-test np-artifact-server "${WORK}/serve"
plant_external_artifact np-test np-artifact \
  "$(artifact_url np-test np-artifact-server artifact.tar.gz)" "$DIGEST"

log "Default-deny all ingress to the artifact server"
kubectl apply -f - <<'EOF'
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: deny-ingress
  namespace: np-test
spec:
  podSelector:
    matchLabels:
      app: np-artifact-server
  policyTypes:
    - Ingress
EOF

ENFORCED=true
if curl_reachable default "$(artifact_url np-test np-artifact-server artifact.tar.gz)"; then
  ENFORCED=false
  log "NOTE: the CNI does not enforce NetworkPolicies (older kindnet) — the deny is a no-op; asserting only the allow-rule positive path."
fi

kubectl apply -f - <<'EOF'
apiVersion: stages.metio.wtf/v1
kind: StageSet
metadata:
  name: np-smoke
  namespace: np-test
spec:
  interval: 1m
  stages:
    - name: app
      sourceRef:
        name: np-artifact
EOF

if [ "$ENFORCED" = true ]; then
  log "Negative: with deny in place the cross-namespace fetch is blocked — np-smoke must not go Ready"
  stays_not_ready stageset np-smoke np-test 6 5
fi

log "Allow ingress from the controller's namespace (${CONTROLLER_NS})"
kubectl apply -f - <<EOF
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: allow-controller-fetch
  namespace: np-test
spec:
  podSelector:
    matchLabels:
      app: np-artifact-server
  policyTypes:
    - Ingress
  ingress:
    - from:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: ${CONTROLLER_NS}
      ports:
        - protocol: TCP
          port: 80
EOF

log "Positive: the allow rule unblocks the fetch — StageSet goes Ready"
wait_ready stageset np-smoke np-test
test "$(kubectl -n np-test get configmap np-applied -o jsonpath='{.data.from}')" \
  = "reached-through-allow-rule" || die "applied ConfigMap content mismatch"

kubectl -n np-test delete stageset np-smoke --timeout=120s || true
log "scenario-networkpolicy PASSED"
