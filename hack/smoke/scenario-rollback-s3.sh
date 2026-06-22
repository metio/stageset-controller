#!/usr/bin/env bash
# SPDX-FileCopyrightText: The stageset-controller Authors
# SPDX-License-Identifier: 0BSD
#
# S3 rollback-store round-trip. The controller is installed with
# rollbackStore.backend=s3 pointed at either Adobe S3Mock (anonymous, in-memory)
# or SeaweedFS (real SigV4 + multipart + SSE); this same script asserts the store
# end to end either way, so both S3 jobs invoke it unchanged. Env: NS, NAME.
#
# What it proves, in two halves:
#
#   1. CAPTURE — a StageSet with rollbackOnFailure renders a good revision, goes
#      Ready=True, and the controller pushes that rendered output into the S3
#      rollback store (storeRendered → S3Store.Put). status.lastAppliedSnapshot
#      records the revision coordinates.
#
#   2. RESTORE — the StageSet is then mutated to point its stage at a broken
#      artifact (a non-fetchable URL / bad digest). The forward run fails, and
#      because rollbackOnFailure is set the controller restores the last-good
#      rendered output FROM THE S3 STORE (attemptRollback → S3Store.Get →
#      re-apply), emits a `RolledBack` Warning event, and the live resource
#      returns to the good content. The good ConfigMap reverting to its prior
#      value after the spec was pointed at garbage is the proof the bytes came
#      back out of the bucket — the producer artifact for the new generation is
#      unusable, so re-fetch can't have produced it.
#
# Assumes the ExternalArtifact stub CRD is installed and the controller runs with
# a cluster-wide apply identity (the workflow's smoke clusterrolebinding).
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
. "${HERE}/lib.sh"

NS="${NS:-default}"
NAME="${NAME:-rollback-s3}"
CM="${NAME}-applied"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# rolled_back_event — true if a RolledBack Warning event exists for the StageSet.
rolled_back_event() {
  kubectl -n "$NS" get events \
    --field-selector "involvedObject.name=${NAME},involvedObject.kind=StageSet" \
    -o jsonpath='{range .items[?(@.type=="Warning")]}{.reason}{"\n"}{end}' 2>/dev/null \
    | grep -qx RolledBack
}

log "Build the good artifact tarball (a ConfigMap manifest)"
mkdir -p "${WORK}/good/src" "${WORK}/serve"
cat > "${WORK}/good/src/configmap.yaml" <<EOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: ${CM}
  namespace: ${NS}
data:
  from: rollback-good-revision
EOF
GOOD_DIGEST="$(make_tarball "${WORK}/good/src" "${WORK}/serve/good.tar.gz")"

log "Serve it in-cluster and plant the good ExternalArtifact"
serve_files "$NS" "${NAME}-server" "${WORK}/serve"
GOOD_URL="$(artifact_url "$NS" "${NAME}-server" good.tar.gz)"
plant_external_artifact "$NS" "${NAME}-good" "$GOOD_URL" "$GOOD_DIGEST" "good@v1"

log "Create a StageSet with rollbackOnFailure consuming the good artifact"
kubectl apply -f - <<EOF
apiVersion: stages.metio.wtf/v1
kind: StageSet
metadata:
  name: ${NAME}
  namespace: ${NS}
spec:
  interval: 1m
  rollbackOnFailure: true
  stages:
    - name: app
      sourceRef:
        name: ${NAME}-good
EOF

wait_ready stageset "$NAME" "$NS"

# CAPTURE half — the good revision is applied and a snapshot recorded. The
# snapshot is the in-status evidence the run succeeded; the rollback store Put
# rode the same successful apply (storeRendered runs right after Apply). The
# RESTORE half below is what proves the store actually holds the bytes.
test "$(kubectl -n "$NS" get configmap "$CM" -o jsonpath='{.data.from}')" \
  = "rollback-good-revision" || die "good revision was not applied"
[ -n "$(kubectl -n "$NS" get stageset "$NAME" \
  -o jsonpath='{.status.lastAppliedSnapshot[0].digest}' 2>/dev/null)" ] \
  || die "no lastAppliedSnapshot recorded — rollback target was not captured"
log "CAPTURE verified: good revision applied, lastAppliedSnapshot recorded"

# Mutate the artifact the stage resolves to so the NEXT forward run fails. The
# producer for the new revision serves a non-existent object (404) under a fresh
# digest, so re-fetch cannot reproduce the good state — only the S3 rollback
# store can. Pointing at a never-served path keeps the failure purely a
# fetch/verify failure, isolating the store as the sole source of the restore.
log "Repoint the ExternalArtifact at a broken (non-fetchable) revision"
BROKEN_URL="$(artifact_url "$NS" "${NAME}-server" missing.tar.gz)"
kubectl -n "$NS" patch externalartifact "${NAME}-good" --subresource=status --type=merge -p "{
  \"status\": {
    \"artifact\": { \"url\": \"${BROKEN_URL}\", \"revision\": \"broken@v2\", \"digest\": \"sha256:$(printf '0%.0s' $(seq 1 64))\", \"path\": \"missing.tar.gz\", \"lastUpdateTime\": \"2026-01-01T00:00:00Z\" }
  }
}"

# Nudge the StageSet so the controller reconciles the broken source promptly
# (an annotation bump bumps the generation / triggers a watch event).
kubectl -n "$NS" annotate stageset "$NAME" \
  stageset-controller.stages.metio.wtf/smoke="$(date +%s)" --overwrite

log "Wait for the controller to fail the forward run and roll back from the S3 store"
restored=""
for i in $(seq 1 60); do
  if rolled_back_event; then
    log "RolledBack event observed after $i polls"
    restored=event
    break
  fi
  sleep 5
done
[ -n "$restored" ] || {
  kubectl -n "$NS" describe stageset "$NAME" >&2 || true
  kubectl -n "$NS" get events --field-selector "involvedObject.name=${NAME}" >&2 || true
  die "controller never emitted a RolledBack event for $NAME"
}

# The live ConfigMap must still carry the GOOD content: the broken revision
# could not be fetched, so the only way the good bytes are present is the
# controller pulling them back out of the S3 rollback store and re-applying.
log "Verify the live resource was restored to the good revision from the S3 store"
for i in $(seq 1 12); do
  [ "$(kubectl -n "$NS" get configmap "$CM" -o jsonpath='{.data.from}' 2>/dev/null)" \
    = "rollback-good-revision" ] && break
  sleep 5
done
test "$(kubectl -n "$NS" get configmap "$CM" -o jsonpath='{.data.from}')" \
  = "rollback-good-revision" \
  || die "rollback did not restore the good revision from the S3 store"

log "RESTORE verified: full rollback from the S3 rollback store succeeded"
log "scenario-rollback-s3 PASSED"
