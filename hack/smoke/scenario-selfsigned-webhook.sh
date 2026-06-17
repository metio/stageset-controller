#!/usr/bin/env bash
# SPDX-FileCopyrightText: The stageset-controller Authors
# SPDX-License-Identifier: 0BSD
#
# Scenario: the self-signed webhook TLS path (cert-manager-free). In this mode
# the controller generates an ECDSA CA + serving cert in-pod, writes them to the
# cert dir, and patches its own named ValidatingWebhookConfiguration's caBundle
# so the apiserver trusts the chain. We prove all three: the named VWC ends up
# with a non-empty caBundle holding a valid x509 cert, and admission then
# succeeds for a StageSet apply — which is impossible unless the apiserver can
# complete the TLS handshake against the controller's self-signed serving cert.
#
# Env: VWC overrides the ValidatingWebhookConfiguration name (default derives
# from the released chart's release name `stageset-controller` in namespace
# `default`, i.e. `stageset-controller-default`). NS / NAME / SRV name the
# objects. Assumes the controller was deployed with
# webhook.enabled=true and webhook.certMode=self-signed.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
. "${HERE}/lib.sh"

NS="${NS:-default}"
NAME="${NAME:-smoke-ss}"
SRV="${SRV:-ss-artifact-server}"
VWC="${VWC:-stageset-controller-default}"

WORK="$(mktemp -d)"
cleanup() {
  kubectl -n "$NS" delete stageset "$NAME" --ignore-not-found --timeout=120s >/dev/null 2>&1 || true
  kubectl -n "$NS" delete externalartifact ss-artifact --ignore-not-found >/dev/null 2>&1 || true
  kubectl -n "$NS" delete deploy,svc "$SRV" --ignore-not-found >/dev/null 2>&1 || true
  kubectl -n "$NS" delete configmap "${SRV}-data" ss-applied --ignore-not-found >/dev/null 2>&1 || true
  rm -rf "$WORK"
}
trap cleanup EXIT

log "Wait for the controller to patch the named VWC's caBundle"
CA=""
for i in $(seq 1 30); do
  CA="$(kubectl get validatingwebhookconfiguration "$VWC" \
    -o jsonpath='{.webhooks[0].clientConfig.caBundle}' 2>/dev/null || true)"
  if [ -n "$CA" ]; then
    log "caBundle populated after $i polls"
    break
  fi
  sleep 2
done
[ -n "$CA" ] || die "controller never patched the VWC $VWC caBundle"

log "Verify the caBundle decodes to a valid x509 certificate"
echo "$CA" | base64 -d | openssl x509 -noout -subject -enddate \
  || die "VWC $VWC caBundle is not a parseable x509 certificate"

log "Build an artifact tarball (a ConfigMap manifest) for the StageSet to render"
mkdir -p "${WORK}/src" "${WORK}/serve"
cat > "${WORK}/src/configmap.yaml" <<EOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: ss-applied
  namespace: ${NS}
data:
  mode: self-signed
EOF
DIGEST="$(make_tarball "${WORK}/src" "${WORK}/serve/artifact.tar.gz")"

log "Serve it in-cluster and plant the ExternalArtifact"
serve_files "$NS" "$SRV" "${WORK}/serve"
plant_external_artifact "$NS" ss-artifact \
  "$(artifact_url "$NS" "$SRV" artifact.tar.gz)" "$DIGEST"

log "Apply a StageSet — admission must pass through the self-signed webhook"
# The caBundle is patched at startup, but the webhook server may need a moment
# to begin listening with the freshly written serving cert. A transient TLS
# refusal during that window is not a failure of the mode, so retry the apply.
applied=""
for i in $(seq 1 30); do
  if kubectl apply -f - <<EOF
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
        name: ss-artifact
EOF
  then
    applied=yes
    log "StageSet admitted after $i attempts"
    break
  fi
  sleep 2
done
[ -n "$applied" ] || die "StageSet apply never succeeded through the self-signed webhook"

wait_ready stageset "$NAME" "$NS"

log "Verify the rendered manifest was applied (end-to-end through the admitted CR)"
test "$(kubectl -n "$NS" get configmap ss-applied -o jsonpath='{.data.mode}')" \
  = "self-signed" || die "applied ConfigMap content mismatch"

log "scenario-selfsigned-webhook PASSED"
