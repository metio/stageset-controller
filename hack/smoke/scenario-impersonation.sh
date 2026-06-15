#!/usr/bin/env bash
# SPDX-FileCopyrightText: The stageset-controller Authors
# SPDX-License-Identifier: 0BSD
#
# Scenario: a stage's apply runs under spec.serviceAccountName, not the
# controller's own (cluster-admin in this smoke) identity. A tenant SA may write
# ConfigMaps but not Secrets:
#   - positive: a ConfigMap-only artifact applies and the StageSet goes Ready;
#   - negative: a Secret artifact is denied, the StageSet reports StageFailed,
#     and the Secret never appears — proving the apply truly impersonates the SA.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
. "${HERE}/lib.sh"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

log "Create tenant namespace + a ConfigMap-only ServiceAccount"
kubectl create namespace tenant
kubectl -n tenant create serviceaccount deployer
kubectl -n tenant create role deployer-configmaps \
  --verb=get,list,watch,create,update,patch,delete --resource=configmaps
kubectl -n tenant create rolebinding deployer-configmaps \
  --role=deployer-configmaps --serviceaccount=tenant:deployer

log "Build two artifacts — a ConfigMap (allowed) and a Secret (denied)"
mkdir -p "${WORK}/cm" "${WORK}/sec" "${WORK}/serve"
cat > "${WORK}/cm/cm.yaml" <<'EOF'
apiVersion: v1
kind: ConfigMap
metadata:
  name: tenant-cm
  namespace: tenant
data:
  from: impersonated-deployer
EOF
cat > "${WORK}/sec/sec.yaml" <<'EOF'
apiVersion: v1
kind: Secret
metadata:
  name: tenant-secret
  namespace: tenant
stringData:
  key: value
EOF
CM_DIGEST="$(make_tarball "${WORK}/cm" "${WORK}/serve/tenant-cm.tar.gz")"
SEC_DIGEST="$(make_tarball "${WORK}/sec" "${WORK}/serve/tenant-secret.tar.gz")"

log "Serve both tarballs and plant their ExternalArtifacts"
serve_files tenant tenant-artifact-server "${WORK}/serve"
plant_external_artifact tenant imp-ok-art \
  "$(artifact_url tenant tenant-artifact-server tenant-cm.tar.gz)" "$CM_DIGEST" "imp-ok@${CM_DIGEST}"
plant_external_artifact tenant imp-denied-art \
  "$(artifact_url tenant tenant-artifact-server tenant-secret.tar.gz)" "$SEC_DIGEST" "imp-denied@${SEC_DIGEST}"

log "Create both StageSets, each impersonating the deployer SA"
kubectl apply -f - <<'EOF'
apiVersion: stages.metio.wtf/v1
kind: StageSet
metadata:
  name: imp-ok
  namespace: tenant
spec:
  interval: 1m
  serviceAccountName: deployer
  stages:
    - name: app
      sourceRef:
        name: imp-ok-art
---
apiVersion: stages.metio.wtf/v1
kind: StageSet
metadata:
  name: imp-denied
  namespace: tenant
spec:
  interval: 1m
  serviceAccountName: deployer
  stages:
    - name: app
      sourceRef:
        name: imp-denied-art
EOF

log "Positive: the granted SA applies the ConfigMap"
wait_ready stageset imp-ok tenant
test "$(kubectl -n tenant get configmap tenant-cm -o jsonpath='{.data.from}')" \
  = "impersonated-deployer" || die "impersonated ConfigMap content mismatch"

log "Negative: the SA cannot create Secrets — apply denied, StageFailed"
wait_reason stageset imp-denied tenant StageFailed 30 5
if kubectl -n tenant get secret tenant-secret 2>/dev/null; then
  die "impersonated SA without Secret RBAC managed to create a Secret"
fi

# Delete the StageSets before returning. imp-denied is a permanent failure that
# otherwise stays in the workqueue's requeue loop; the controller runs a single
# worker, so leaving it behind would compete with later scenarios.
log "Clean up tenant StageSets"
kubectl -n tenant delete stageset imp-ok imp-denied --timeout=120s --ignore-not-found || true
log "scenario-impersonation PASSED"
