#!/usr/bin/env bash
# SPDX-FileCopyrightText: The stageset-controller Authors
# SPDX-License-Identifier: 0BSD
#
# Scenario: the chart's NetworkPolicy is actually ENFORCED by a real CNI — what
# scenario-networkpolicy.sh cannot test, because kind's default kindnet treats
# NetworkPolicy as a no-op. The calling workflow installs an enforcing CNI
# (Calico / Cilium) and the chart with networkPolicy.enabled=true,
# networkPolicy.engine matching the CNI, and networkPolicy.gate.from scoped to an
# admitted namespace. It is deliberately engine-AGNOSTIC: it asserts traffic
# behaviour against the gate port, not a specific policy object kind.
#
# stageset-controller is a CONSUMER, not a producer — it serves no artifact
# storage. The enforceable, from-scoped port is the read-only Flagger stage-gate
# endpoint (ports.gate, 8082), exposed by the chart's gate Service. Curling it
# returns a normal HTTP code when the connection is admitted (the gate app
# answers — 404 for an unknown path) and fails to connect when denied.
#
# It pins:
#   1. the operator came up under the rendered policy + real CNI — proven by the
#      calling workflow's `helm --wait` (the deployment never goes ready if the
#      policy breaks the controller's own traffic), so this script needs no CR;
#   2. ALLOW: the gate port is reachable from the admitted namespace
#      (GATE_FROM_NS) — the allowlist re-permits the scoped caller under
#      enforcement;
#   3. DENY (ENFORCE=1): the gate port is NOT reachable from a non-admitted
#      namespace. The chart's kubernetes-engine allowlist scopes the gate port to
#      GATE_FROM_NS, so under a real CNI a request from another namespace is
#      dropped. This is the true-negative the kindnet job can't make.
#
# ENFORCE=0 keeps only the allow assertion. It is used where the rendered dialect
# is pod-scoped allow-all on the required ports (the chart's cilium / calico
# engines apply no per-source filter, so there is no per-source deny to assert),
# and for the clusterNetworkPolicy engine whose v1alpha2 API has no on-kind
# enforcer yet (the CRDs are installed so the objects apply and the operator path
# is exercised — apply-level validation).
#
# Env: STAGESET_NS (install namespace; default stageset-system), GATE_SVC (gate
# Service name; default stageset-controller-gate), GATE_PORT (default 8082),
# GATE_FROM_NS (admitted namespace; default flux-system), DENY_NS (a
# non-admitted namespace; default default), ENFORCE (1 = also assert deny, 0 =
# allow only).
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
. "${HERE}/lib.sh"

STAGESET_NS="${STAGESET_NS:-stageset-system}"
GATE_SVC="${GATE_SVC:-stageset-controller-gate}"
GATE_PORT="${GATE_PORT:-8082}"
GATE_FROM_NS="${GATE_FROM_NS:-flux-system}"
DENY_NS="${DENY_NS:-default}"
ENFORCE="${ENFORCE:-1}"

URL="http://${GATE_SVC}.${STAGESET_NS}:${GATE_PORT}/"
log "install namespace: $STAGESET_NS  gate URL=$URL  admitted ns=$GATE_FROM_NS  ENFORCE=$ENFORCE"

# Make sure the admitted namespace exists (flux-system is created by setup-flux
# in the producer repos, but this scenario installs no Flux).
kubectl get namespace "$GATE_FROM_NS" >/dev/null 2>&1 || kubectl create namespace "$GATE_FROM_NS"

log "ALLOW: the gate port must be reachable from the admitted namespace ($GATE_FROM_NS)"
if curl_reachable "$GATE_FROM_NS" "$URL"; then
  log "gate port reachable from $GATE_FROM_NS (allowlist admits it)"
else
  # curl_reachable uses -f, so a gate 404 counts as "not reachable". Re-probe
  # without -f: ANY HTTP response from the gate app proves the connection was
  # admitted (the gate answers 404 for an unknown path).
  code="$(kubectl -n "$GATE_FROM_NS" run "smoke-allow-$$" --image="$CURL_IMAGE" \
    --restart=Never --rm -i --command -- \
    curl -s -o /dev/null -w '%{http_code}' --connect-timeout 8 --max-time 15 "$URL" 2>/dev/null || echo "000")"
  log "admitted-namespace probe got HTTP $code"
  [ "$code" = "000" ] && die "gate port NOT reachable from $GATE_FROM_NS — the allowlist over-denied the admitted namespace"
fi

if [ "$ENFORCE" = "1" ]; then
  log "DENY: the gate port must be BLOCKED from a non-admitted namespace ($DENY_NS)"
  gate_denied "$URL" "$DENY_NS"
else
  log "ENFORCE=0: skipping the deny assertion (dialect is allow-all on the ports, or has no on-kind enforcer)"
fi

log "scenario-networkpolicy-enforcement PASSED (ENFORCE=$ENFORCE)"
