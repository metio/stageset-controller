---
title: Manager unavailable
description: The controller process is running, but its manager cannot start, so no StageSet is being reconciled
tags: [runbooks, troubleshooting, operations, networking, rbac]
---

Linked from the `stageset_manager_available == 0` signal and from `GET /manager`
returning `503`. The pod is up and its probe and metrics endpoints answer; the
controller manager is what cannot start.

## Symptom

```text
stageset_manager_available 0
```

```shell
kubectl --namespace <ns> port-forward deploy/<release> 8081:8081 &
curl --silent http://127.0.0.1:8081/manager
# {"status":"unavailable","reason":"create manager: Get \"https://10.24.64.1:443/api\": dial tcp 10.24.64.1:443: i/o timeout","since":"2026-09-24T09:14:02Z","attempts":7}
```

- Every `StageSet` stays at its last status; new ones stay `Ready=Unknown`.
  Nothing reconciles, no stage applies, no action runs.
- The pod is `Running` and `0/1`. It does **not** restart: `/healthz` is
  unconditional, and the manager is retried in place.
- The stage-gate endpoint, the MCP server and the admission webhook are all
  manager-scoped, so they are down too. The apiserver sees no webhook endpoints
  rather than a refused connection.

## Cause

Everything the manager needs from the apiserver happens while it is being built —
the discovery behind each reconciler's watches, the RESTMapper lookups the
producer watches are gated on — so anything that blocks those calls keeps the
controller down:

1. **Egress to the apiserver is denied.** A cluster-wide default-deny
   NetworkPolicy with no matching allow rule is the common case. The reason
   string names the apiserver address and ends in `i/o timeout` or
   `connection refused`. See [Network policy](/security/network-policy/).
2. **The controller's ClusterRole is incomplete.** The reason carries
   `forbidden`, naming the verb and resource.
3. **The CRDs are not installed.** The reason names the missing kind. A chart
   install with `crds.create=false` and no out-of-band apply lands here.
4. **The webhook has no certificate yet.** The reason is
   `open …/serving-certs/tls.crt: no such file or directory` — cert-manager has
   not issued the Secret, or the mount is wrong. The manager syncs its cache and
   then fails on the webhook server, so this one alternates between available and
   unavailable until the certificate lands.
5. **The apiserver is genuinely unreachable** — control-plane outage, in-cluster
   DNS failure, a mesh sidecar that has not started yet.
6. **The leader-election lease was lost** after a period of healthy operation.
   The reason is `leader election lost`; the next manager blocks on acquiring the
   lease again, which is the behaviour the lease exists for. Repeated losses point
   at apiserver latency or a clock problem, not at the controller.

## Diagnosis

```shell
# The reason, verbatim, with how long it has been true and how many starts.
curl --silent http://127.0.0.1:8081/manager

# The same reason in the logs, once per fresh cause plus one per retry.
kubectl --namespace <ns> logs deploy/<release> | grep -i "manager unavailable\|still unavailable"

# Is it reachability? Resolve and dial the apiserver from the pod's namespace.
kubectl --namespace <ns> get networkpolicy
kubectl --namespace default get endpoints kubernetes

# Is it the webhook certificate?
kubectl --namespace <ns> get certificate,secret
```

An `i/o timeout` against the address that `get endpoints kubernetes` reports,
with a default-deny policy in the namespace, is cause 1. A `forbidden` naming a
resource is cause 2. A `no matches for kind` is cause 3. A missing `tls.crt` is
cause 4.

## Remediation

Fix the cause; no restart is needed. The manager is retried on a backoff that
caps at five minutes, so a repaired cluster recovers within that at worst, and
`stageset_manager_available` returns to `1` on its own.

For denied egress, admit the apiserver — the chart renders that rule for you once
`networkPolicy.egress.enabled` is set; see
[Network policy](/security/network-policy/).

For an incomplete ClusterRole, reinstall or upgrade the chart so the generated
roles match the running binary, then confirm:

```shell
kubectl auth can-i --as=system:serviceaccount:<ns>:<release> \
  list stagesets.stages.metio.wtf --all-namespaces
```

For missing CRDs, apply them and wait — the manager picks them up on its next
attempt:

```shell
kubectl apply --server-side --filename config/crd/
```

For a missing webhook certificate, check the Issuer and Certificate are ready, or
switch to `webhook.certMode=self-signed`, which needs no cert-manager.

## Related

- [Degraded manager](/running/operations/#degraded-manager) — the supervision
  behaviour and the signals it publishes.
- [Workqueue saturation](/runbooks/workqueue-saturation/) — the manager is up but
  cannot keep up, which is a different failure.
- [webhook-cert-renewal](/runbooks/webhook-cert-renewal/) — a caBundle write that
  keeps failing after the manager is up.
