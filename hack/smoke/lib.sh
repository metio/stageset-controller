# SPDX-FileCopyrightText: The stageset-controller Authors
# SPDX-License-Identifier: 0BSD
#
# shellcheck shell=bash
#
# Shared helpers for the StageSet end-to-end smoke scenarios. Sourced by the
# scenario-*.sh scripts. These encode controller BEHAVIOUR (plant an
# ExternalArtifact, create a StageSet, wait for a status, verify the applied
# objects) and are deliberately agnostic to HOW the controller was deployed —
# the calling workflow owns that (which manifests/chart, which image). The
# stageset-controller repo runs them against the dev binary + released chart;
# the helm-charts repo runs the same scripts (checked out from this repo at the
# released tag) against the dev chart + released binary. Assumes kubectl is
# already pointed at the target cluster and the controller is deployed.
#
# The artifact data plane is faked with an in-cluster static file server (no
# live source-controller needed): a tarball's bytes are baked into a ConfigMap,
# served over plain HTTP, and pointed at by an ExternalArtifact whose
# status.artifact carries the matching digest — exactly the resolve -> fetch ->
# digest-verify -> build -> apply pipeline a real producer (e.g. jaas) drives.

set -euo pipefail

log() { printf '\n=== %s ===\n' "$*" >&2; }
die() { printf 'SMOKE FAIL: %s\n' "$*" >&2; exit 1; }

# Pinned, long-form image references (per repo convention).
PY_IMAGE="docker.io/library/python:3.13-alpine"
CURL_IMAGE="docker.io/curlimages/curl:8.10.1"

# make_tarball <content-dir> <out-tarball> — tar+gzip the directory's contents
# (sorted, so the digest is stable) and echo the "sha256:<hex>" digest the
# ExternalArtifact status must advertise for fetch-time verification to pass.
make_tarball() {
  local dir=$1 out=$2
  tar czf "$out" -C "$dir" .
  printf 'sha256:%s\n' "$(sha256sum "$out" | cut -d' ' -f1)"
}

# serve_files <ns> <server-name> <dir-of-tarballs> — bake every file in <dir>
# into a ConfigMap and serve them over HTTP from a one-replica Deployment +
# Service named <server-name> in <ns>. Each file is reachable at
# http://<server-name>.<ns>.svc.cluster.local/<filename>. kubectl stores binary
# --from-file content under binaryData (base64); the volume mount decodes it
# back to the original tarball bytes.
serve_files() {
  local ns=$1 name=$2 dir=$3 f
  local args=()
  for f in "$dir"/*; do args+=( "--from-file=$f" ); done
  kubectl -n "$ns" create configmap "${name}-data" "${args[@]}"
  kubectl apply -f - <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ${name}
  namespace: ${ns}
spec:
  replicas: 1
  selector:
    matchLabels:
      app: ${name}
  template:
    metadata:
      labels:
        app: ${name}
    spec:
      containers:
        - name: server
          image: ${PY_IMAGE}
          command: ["python", "-m", "http.server", "80", "--directory", "/srv"]
          ports:
            - containerPort: 80
          volumeMounts:
            - name: data
              mountPath: /srv
      volumes:
        - name: data
          configMap:
            name: ${name}-data
---
apiVersion: v1
kind: Service
metadata:
  name: ${name}
  namespace: ${ns}
spec:
  selector:
    app: ${name}
  ports:
    - port: 80
      targetPort: 80
EOF
  kubectl -n "$ns" rollout status "deploy/${name}" --timeout=120s
}

# artifact_url <ns> <server-name> <filename> — the in-cluster URL serve_files
# exposes a tarball at.
artifact_url() {
  printf 'http://%s.%s.svc.cluster.local/%s\n' "$2" "$1" "$3"
}

# plant_external_artifact <ns> <name> <url> <digest> [revision] — create an
# ExternalArtifact and stamp a Ready=True status pointing at <url>/<digest>, so
# the resolver treats it as a consumable source. lastTransitionTime is a fixed
# constant — the value is irrelevant to resolution, only its presence is.
plant_external_artifact() {
  local ns=$1 name=$2 url=$3 digest=$4 rev=${5:-smoke@$4}
  kubectl apply -f - <<EOF
apiVersion: source.toolkit.fluxcd.io/v1
kind: ExternalArtifact
metadata:
  name: ${name}
  namespace: ${ns}
spec: {}
EOF
  kubectl -n "$ns" patch externalartifact "$name" --subresource=status --type=merge -p "{
    \"status\": {
      \"artifact\": { \"url\": \"${url}\", \"revision\": \"${rev}\", \"digest\": \"${digest}\" },
      \"conditions\": [{
        \"type\": \"Ready\", \"status\": \"True\", \"reason\": \"Succeeded\",
        \"message\": \"artifact ready\", \"lastTransitionTime\": \"2026-01-01T00:00:00Z\"
      }]
    }
  }"
}

# plant_flux_source <kind> <ns> <name> <url> <digest> [revision] — create a
# classic Flux source (GitRepository/OCIRepository/Bucket) and stamp a Ready
# status.artifact, exactly like an ExternalArtifact, so the resolver consumes it
# directly as a stage source.
plant_flux_source() {
  local kind=$1 ns=$2 name=$3 url=$4 digest=$5 rev=${6:-smoke@$5} plural
  case "$kind" in
    GitRepository) plural=gitrepositories ;;
    OCIRepository) plural=ocirepositories ;;
    Bucket) plural=buckets ;;
    *) die "plant_flux_source: unknown kind $kind" ;;
  esac
  kubectl apply -f - <<EOF
apiVersion: source.toolkit.fluxcd.io/v1
kind: ${kind}
metadata:
  name: ${name}
  namespace: ${ns}
spec: {}
EOF
  kubectl -n "$ns" patch "$plural" "$name" --subresource=status --type=merge -p "{
    \"status\": {
      \"artifact\": { \"url\": \"${url}\", \"revision\": \"${rev}\", \"digest\": \"${digest}\" },
      \"conditions\": [{
        \"type\": \"Ready\", \"status\": \"True\", \"reason\": \"Succeeded\",
        \"message\": \"artifact ready\", \"lastTransitionTime\": \"2026-01-01T00:00:00Z\"
      }]
    }
  }"
}

# ready_status <kind> <name> <ns> — echoes the Ready condition's status (or "").
ready_status() {
  kubectl -n "$3" get "$1" "$2" \
    -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true
}

# ready_reason <kind> <name> <ns> — echoes the Ready condition's reason (or "").
ready_reason() {
  kubectl -n "$3" get "$1" "$2" \
    -o jsonpath='{.status.conditions[?(@.type=="Ready")].reason}' 2>/dev/null || true
}

# wait_ready <kind> <name> <ns> [polls] [sleep] — block until Ready=True.
wait_ready() {
  local kind=$1 name=$2 ns=$3 polls=${4:-60} s=${5:-5} i
  for i in $(seq 1 "$polls"); do
    [ "$(ready_status "$kind" "$name" "$ns")" = "True" ] && { log "$kind/$name Ready=True after $i polls"; return 0; }
    sleep "$s"
  done
  kubectl -n "$ns" describe "$kind" "$name" >&2 || true
  die "$kind/$name did not reach Ready=True"
}

# wait_reason <kind> <name> <ns> <reason> [polls] [sleep] — block until the
# Ready condition's reason equals <reason>.
wait_reason() {
  local kind=$1 name=$2 ns=$3 want=$4 polls=${5:-60} s=${6:-2} i
  for i in $(seq 1 "$polls"); do
    [ "$(ready_reason "$kind" "$name" "$ns")" = "$want" ] && { log "$kind/$name Ready reason=$want after $i polls"; return 0; }
    sleep "$s"
  done
  kubectl -n "$ns" describe "$kind" "$name" >&2 || true
  die "$kind/$name Ready reason never became $want"
}

# stays_not_ready <kind> <name> <ns> [polls] [sleep] — assert the object does NOT
# reach Ready=True for the whole window, then return. Fails fast if it flips to
# Ready=True. Used for the blocked-fetch negative: a NetworkPolicy deny drops
# packets, so the fetch hangs up to the HTTP client timeout rather than failing
# fast — we can't wait for a specific failure reason in a bounded window, but we
# CAN assert the StageSet never goes Ready while the path is blocked. The Ready
# flip after the allow rule is applied is the positive half of the proof.
stays_not_ready() {
  local kind=$1 name=$2 ns=$3 polls=${4:-6} s=${5:-5} i
  for i in $(seq 1 "$polls"); do
    [ "$(ready_status "$kind" "$name" "$ns")" = "True" ] && {
      kubectl -n "$ns" describe "$kind" "$name" >&2 || true
      die "$kind/$name reached Ready=True while ingress to the artifact server was denied"
    }
    sleep "$s"
  done
  log "$kind/$name stayed non-Ready under the deny policy ($polls polls)"
}

# curl_reachable <ns> <url> [max_time] — true (0) if a throwaway pod can GET the
# URL within max_time seconds, false otherwise. Used to detect whether the
# cluster's CNI actually enforces NetworkPolicies before asserting that a
# default-deny policy blocks a fetch (kindnet enforces on recent versions; older
# treats policies as no-ops).
curl_reachable() {
  local ns=$1 url=$2 t=${3:-8}
  kubectl -n "$ns" run "smoke-probe-$$" --image="$CURL_IMAGE" \
    --restart=Never --rm -i --command -- \
    curl -fsS --max-time "$t" -o /dev/null "$url" >/dev/null 2>&1
}

# gate_denied <url> <from-ns> — fail unless reaching <url> from a throwaway pod
# in <from-ns> is BLOCKED. The negative counterpart to a reachable probe: under
# an enforcing CNI the chart's gate-port allowlist (networkPolicy.gate.from)
# admits only the named source namespaces, so a request from any other namespace
# must be dropped. A SUCCESSFUL request means the allowlist is NOT enforcing. The
# short connect timeout makes a dropped dial fail fast instead of hanging on the
# default timeout; the curl image is pinned and long-form per repo convention.
# Note "success" is ANY HTTP response (the gate app answers 404 for an unknown
# path when reachable) — curl without -f returns 0 on a 404, so the deny is
# proven only when curl itself fails to connect.
gate_denied() {
  local url=$1 ns=$2
  if kubectl -n "$ns" run --rm -i --restart=Never \
      --image="$CURL_IMAGE" "smoke-deny-$$" \
      -- curl -sS --connect-timeout 8 --max-time 15 "$url" -o /dev/null; then
    die "gate port was reachable from $ns — the gate-port allowlist is NOT enforcing"
  fi
  log "gate port correctly BLOCKED from $ns (gate-port allowlist enforced)"
}

# deploy_meshed_curl <ns> <name> [serviceAccount] — deploy a long-running curl
# Deployment in <ns> under the given ServiceAccount (default "default") and wait
# until it is ready. The service-mesh scenarios curl through the pod's sidecar
# via `kubectl exec` rather than `kubectl run`, because an injected sidecar never
# lets a one-shot pod complete (the proxy keeps running). <ns> must already be
# mesh-injected so the pod gets a sidecar and a workload identity. The SA is what
# the mesh authz keys on — both Istio (source.principals SPIFFE id) and Linkerd
# (MeshTLSAuthentication identity) authorize by the pod's ServiceAccount.
deploy_meshed_curl() {
  local ns=$1 name=$2 sa=${3:-default}
  kubectl -n "$ns" apply -f - <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ${name}
  namespace: ${ns}
  labels: { app: ${name} }
spec:
  replicas: 1
  selector: { matchLabels: { app: ${name} } }
  template:
    metadata: { labels: { app: ${name} } }
    spec:
      serviceAccountName: ${sa}
      containers:
        - name: curl
          image: ${CURL_IMAGE}
          command: ["sleep", "infinity"]
EOF
  kubectl -n "$ns" rollout status deploy/"$name" --timeout=180s
}

# meshed_http_status <ns> <deploy> <url> — echo the HTTP status code curl gets
# hitting <url> from inside the meshed <deploy> (so the request traverses the
# sidecar and the target's inbound mesh authz). "000" means the connection
# failed outright (e.g. a Linkerd reset), which counts as a rejection.
meshed_http_status() {
  local ns=$1 deploy=$2 url=$3
  kubectl -n "$ns" exec "deploy/$deploy" -c curl -- \
    curl -s -o /dev/null -w '%{http_code}' --max-time 15 "$url" 2>/dev/null || echo "000"
}
