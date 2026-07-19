#!/usr/bin/env bash
# SPDX-FileCopyrightText: The stageset-controller Authors
# SPDX-License-Identifier: 0BSD
#
# Scenario: end-to-end image-verification gate. Proves the one path unit and
# envtest coverage cannot — a real cosign signature on a real registry image,
# discovered and verified through go-containerregistry before a stage applies it.
#
#   1. Push two distinct images to an in-cluster registry; cosign-sign only one
#      (public-key, new bundle format, no Rekor — matching the key-authority path).
#   2. Create an ImageVerificationPolicy carrying the public key inline.
#   3. A StageSet rendering the SIGNED image reaches Ready and its applied
#      Deployment is rewritten to the verified digest (registry/app@sha256:…).
#   4. A StageSet rendering the UNSIGNED image is held at ImageUnverified and its
#      Deployment is never applied.
#
# Requires docker + cosign + kubectl on the host. The controller must already run
# with --image-verification-insecure-registry=<REGISTRY_HOST> (the registry is
# plain HTTP), which the calling workflow patches in. Deployments run zero replicas,
# so the kubelet never pulls from the insecure registry — only the controller does,
# to verify.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
. "${HERE}/lib.sh"

NS="${NS:-default}"
REGISTRY_NS="${REGISTRY_NS:-image-verify}"
# The in-cluster registry host the controller reaches and the image refs embed.
# Must match the controller's --image-verification-insecure-registry arg.
REGISTRY_HOST="${REGISTRY_HOST:-registry.image-verify.svc.cluster.local:5000}"
# Two distinct base images so the signed and unsigned refs have different digests
# (a shared digest would carry the same signature and defeat the negative case).
SIGNED_BASE="${SIGNED_BASE:-docker.io/library/busybox:1.37}"
UNSIGNED_BASE="${UNSIGNED_BASE:-docker.io/library/alpine:3.21}"

WORK="$(mktemp -d)"
PF_PID=""
cleanup() {
  [ -n "$PF_PID" ] && kill "$PF_PID" >/dev/null 2>&1 || true
  kubectl -n "$NS" delete stageset iv-signed iv-unsigned --ignore-not-found --timeout=120s >/dev/null 2>&1 || true
  kubectl delete imageverificationpolicy iv-smoke --ignore-not-found >/dev/null 2>&1 || true
  rm -rf "$WORK"
}
trap cleanup EXIT

log "Port-forward to the in-cluster registry"
kubectl -n "$REGISTRY_NS" port-forward svc/registry 5000:5000 >/dev/null 2>&1 &
PF_PID=$!
for i in $(seq 1 30); do
  curl -fsS http://localhost:5000/v2/ >/dev/null 2>&1 && { log "registry reachable after $i tries"; break; }
  [ "$i" = 30 ] && die "registry did not become reachable over the port-forward"
  sleep 1
done

log "Push a signed and an unsigned image (distinct digests)"
docker pull "$SIGNED_BASE"
docker tag "$SIGNED_BASE" localhost:5000/app:signed
docker push localhost:5000/app:signed
docker pull "$UNSIGNED_BASE"
docker tag "$UNSIGNED_BASE" localhost:5000/app:unsigned
docker push localhost:5000/app:unsigned

SIGNED_DIGEST="$(docker inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' localhost:5000/app:signed \
  | grep '^localhost:5000/app@' | head -1 | cut -d@ -f2)"
[ -n "$SIGNED_DIGEST" ] || die "could not resolve the pushed signed-image digest"
log "Signed image digest: ${SIGNED_DIGEST}"

log "Generate a cosign key pair and sign the signed image (new bundle format, no Rekor)"
export COSIGN_PASSWORD=""
( cd "$WORK" && cosign generate-key-pair )
# registry-referrers-mode=oci-1-1 stores the bundle as an OCI 1.1 referrer (native
# API + the spec's fallback tag), which is what go-containerregistry's Referrers()
# reads. cosign's default "legacy" mode writes a .sig tag the verifier does not look
# at.
cosign sign --yes \
  --key "${WORK}/cosign.key" \
  --new-bundle-format \
  --registry-referrers-mode=oci-1-1 \
  --tlog-upload=false \
  --allow-insecure-registry \
  "localhost:5000/app@${SIGNED_DIGEST}"

log "Create the ImageVerificationPolicy with the public key inline"
{
  cat <<EOF
apiVersion: stages.metio.wtf/v1
kind: ImageVerificationPolicy
metadata:
  name: iv-smoke
spec:
  images:
    - "${REGISTRY_HOST}/**"
  authorities:
    - key:
        publicKey: |
EOF
  sed 's/^/          /' "${WORK}/cosign.pub"
} | kubectl apply -f -

log "Build artifact tarballs — a Deployment per image (zero replicas: no kubelet pull)"
mkdir -p "${WORK}/signed" "${WORK}/unsigned" "${WORK}/serve"
gen_deploy() {
  local file=$1 name=$2 image=$3
  cat > "$file" <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ${name}
  namespace: ${NS}
spec:
  replicas: 0
  selector:
    matchLabels: { app: ${name} }
  template:
    metadata:
      labels: { app: ${name} }
    spec:
      containers:
        - name: app
          image: ${image}
EOF
}
gen_deploy "${WORK}/signed/deploy.yaml" iv-signed-app "${REGISTRY_HOST}/app:signed"
gen_deploy "${WORK}/unsigned/deploy.yaml" iv-unsigned-app "${REGISTRY_HOST}/app:unsigned"
SIGNED_TAR="$(make_tarball "${WORK}/signed" "${WORK}/serve/signed.tar.gz")"
UNSIGNED_TAR="$(make_tarball "${WORK}/unsigned" "${WORK}/serve/unsigned.tar.gz")"

log "Serve the tarballs and plant their ExternalArtifacts"
serve_files "$NS" iv-server "${WORK}/serve"
plant_external_artifact "$NS" iv-signed-src "$(artifact_url "$NS" iv-server signed.tar.gz)" "$SIGNED_TAR"
plant_external_artifact "$NS" iv-unsigned-src "$(artifact_url "$NS" iv-server unsigned.tar.gz)" "$UNSIGNED_TAR"

apply_stageset() {
  local name=$1 src=$2
  kubectl apply -f - <<EOF
apiVersion: stages.metio.wtf/v1
kind: StageSet
metadata:
  name: ${name}
  namespace: ${NS}
spec:
  interval: 1m
  stages:
    - name: app
      sourceRef:
        name: ${src}
      readyChecks:
        disableWait: true
EOF
}

log "SIGNED StageSet must verify, pin, and apply"
apply_stageset iv-signed iv-signed-src
wait_ready stageset iv-signed "$NS"
APPLIED_IMAGE="$(kubectl -n "$NS" get deploy iv-signed-app -o jsonpath='{.spec.template.spec.containers[0].image}')"
case "$APPLIED_IMAGE" in
  "${REGISTRY_HOST}/app@sha256:"*) log "applied image digest-pinned: ${APPLIED_IMAGE}" ;;
  *) die "signed image was not digest-pinned (got '${APPLIED_IMAGE}')" ;;
esac

log "UNSIGNED StageSet must be held at ImageUnverified and never applied"
apply_stageset iv-unsigned iv-unsigned-src
wait_reason stageset iv-unsigned "$NS" ImageUnverified
if kubectl -n "$NS" get deploy iv-unsigned-app >/dev/null 2>&1; then
  die "an unverified image was applied (Deployment iv-unsigned-app exists)"
fi
log "unsigned image correctly held before apply"

log "scenario-image-verify PASSED"
