#!/usr/bin/env bash
# SPDX-FileCopyrightText: The stageset-controller Authors
# SPDX-License-Identifier: 0BSD
#
# Scenario: error-budget freeze via a webhook metric source. Stands up an
# in-cluster stub serving an SLO "remaining budget" JSON, and a StageSet whose
# spec.errorBudget reads it over a webhook source. While the stub reports an
# exhausted budget the rollout is frozen (Ready=False/BudgetExhausted) and the
# stage's object is NOT applied; once the stub reports a healthy budget the
# rollout proceeds and the object appears. Exercises the webhook MetricSource
# provider and the error-budget gate end-to-end against a real controller.
# Assumes the controller is deployed and the ExternalArtifact CRD is installed
# (webhook not required).
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
. "${HERE}/lib.sh"

NS="${NS:-default}"
NAME="${NAME:-budget-demo}"
METRIC="${NAME}-metric"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"; kubectl -n "$NS" delete stageset "$NAME" --ignore-not-found --timeout=120s >/dev/null 2>&1 || true' EXIT

# serve_metric <remaining> — (re)serve the budget JSON the webhook source reads,
# then restart the stub so the new value is picked up immediately (a ConfigMap
# volume update would otherwise lag behind by the kubelet sync period).
serve_metric() {
  local remaining=$1
  printf '{"remaining": %s}\n' "$remaining" > "${WORK}/metric/budget.json"
  kubectl -n "$NS" create configmap "${METRIC}-data" \
    --from-file="${WORK}/metric/budget.json" --dry-run=client -o yaml | kubectl apply -f -
  kubectl -n "$NS" rollout restart "deploy/${METRIC}" >/dev/null 2>&1 || true
  kubectl -n "$NS" rollout status "deploy/${METRIC}" --timeout=120s
}

log "Build + serve the stage artifact (a ConfigMap manifest)"
mkdir -p "${WORK}/src" "${WORK}/serve" "${WORK}/metric"
cat > "${WORK}/src/configmap.yaml" <<EOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: ${NAME}-applied
  namespace: ${NS}
data:
  from: stageset-budget-smoke
EOF
DIGEST="$(make_tarball "${WORK}/src" "${WORK}/serve/artifact.tar.gz")"
serve_files "$NS" "${NAME}-server" "${WORK}/serve"
plant_external_artifact "$NS" "${NAME}-artifact" \
  "$(artifact_url "$NS" "${NAME}-server" artifact.tar.gz)" "$DIGEST"

log "Serve the metric stub reporting an EXHAUSTED budget (remaining=0)"
printf '{"remaining": 0}\n' > "${WORK}/metric/budget.json"
serve_files "$NS" "$METRIC" "${WORK}/metric"

log "Create a StageSet frozen on its error budget (webhook source)"
kubectl apply -f - <<EOF
apiVersion: stages.metio.wtf/v1
kind: StageSet
metadata:
  name: ${NAME}
  namespace: ${NS}
spec:
  interval: 30s
  errorBudget:
    source:
      webhook:
        url: http://${METRIC}.${NS}.svc.cluster.local/budget.json
        jsonPath: "{.remaining}"
    freezeThreshold: "0.1"
    interval: 15s
  stages:
    - name: app
      sourceRef:
        name: ${NAME}-artifact
EOF

log "While out of budget, the rollout must be frozen and apply nothing"
wait_reason stageset "$NAME" "$NS" BudgetExhausted
if kubectl -n "$NS" get configmap "${NAME}-applied" >/dev/null 2>&1; then
  die "an exhausted error budget must hold the rollout (ConfigMap was applied)"
fi
[ "$(kubectl -n "$NS" get stageset "$NAME" -o jsonpath='{.status.budgetFreeze.remaining}')" = "0" ] \
  || die "status.budgetFreeze.remaining should record the observed value"
log "Rollout is frozen with status.budgetFreeze recorded"

log "Recover the budget (remaining=0.9) — the rollout must proceed"
serve_metric 0.9
wait_ready stageset "$NAME" "$NS"
test "$(kubectl -n "$NS" get configmap "${NAME}-applied" -o jsonpath='{.data.from}')" \
  = "stageset-budget-smoke" || die "the rollout did not apply after the budget recovered"

log "scenario-errorbudget PASSED"
