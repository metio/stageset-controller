---
title: Network policy
description: The opt-in NetworkPolicy the chart ships — pod-scoped allowlists vs. a namespace-wide default-deny, choosing a policy engine, the ingress and egress traffic the controller needs, and how to tighten each port.
tags: [network-policy, security, networking]
---

The Helm chart ships an opt-in `NetworkPolicy` for the controller pod. It is off by
default and renders only when `networkPolicy.enabled` is `true`. Two independent
layers are on offer: pod-scoped allowlists that lock down only the controller's own
pods (the safe default), and an additional namespace-wide default-deny for a
zero-trust namespace. The ingress and egress tables below describe exactly what
traffic the controller depends on, so everything else can be denied.

## Two layers: pod-scoped allowlists vs. namespace default-deny

`networkPolicy.enabled: true` renders per-workload, **pod-scoped allowlist**
policies. They select only the controller's own pods through their selector labels
and lock down just those pods to the required ports. This is the safe default and is
fine in a shared namespace: co-located workloads — including anything in
`flux-system` if the controller shares that namespace — are untouched.

`networkPolicy.defaultDeny.enabled` (default `false`) **additionally** renders a
namespace-wide default-deny so every pod in the namespace is denied by default and
the allowlists become the only exceptions (a zero-trust namespace). The default-deny
sits at a lower precedence than the allowlists, so the allowlists always win for the
controller pods while everything else is denied.

Pick the layer that matches namespace ownership:

- **`defaultDeny.enabled: false`** (default) — pod-scoped setup. Only the
  controller's pods are locked down; neighbours keep whatever posture their own
  policies give them.
- **`defaultDeny.enabled: true`** — namespace zero-trust. Enable this **only when
  the controller owns its namespace**, because the deny-all also denies every
  co-located workload that does not have its own allowing policy.

`defaultDeny.order` (default `2000`) tunes the Calico `order` / ClusterNetworkPolicy
`priority` that keeps the deny-all subordinate to the allowlists. The `kubernetes`
and `cilium` engines have no precedence knob — deny and allow combine additively and
allow wins — so the value matters only for the `calico` and `clusterNetworkPolicy`
engines.

```yaml
networkPolicy:
  enabled: true
  defaultDeny:
    enabled: true   # only when the controller owns this namespace
    order: 2000
```

## Choosing a policy engine

`networkPolicy.engine` selects which policy dialect the chart renders. It is
explicit, not auto-detected: a chart that sniffed the running CNI would render
different objects on different clusters from identical values, which breaks GitOps
determinism. You name the engine, and the rendered manifest is the same everywhere.

| `engine` | Renders | API | FQDN egress |
| --- | --- | --- | --- |
| `kubernetes` (default) | `NetworkPolicy` | `networking.k8s.io/v1` | No |
| `cilium` | `CiliumNetworkPolicy` | `cilium.io/v2` | Yes — free `toFQDNs` egress |
| `calico` | `NetworkPolicy` | `projectcalico.org/v3` | No — OSS Calico has no FQDN egress; that is Calico Enterprise only |
| `clusterNetworkPolicy` | `ClusterNetworkPolicy` | `policy.networking.k8s.io/v1alpha2` | No |

`clusterNetworkPolicy` renders the SIG-Network `ClusterNetworkPolicy` that
consolidates the deprecated `AdminNetworkPolicy` + `BaselineAdminNetworkPolicy`
APIs into one resource. It is alpha, cluster-scoped, and rendered in the `Baseline`
tier so a developer-authored `NetworkPolicy` still takes precedence over it.

```yaml
networkPolicy:
  enabled: true
  engine: cilium
```

The per-port `.from` knobs documented under [Configuring ingress](#configuring-ingress)
apply to the `kubernetes` engine only. For the other engines the allowlists are
pod-scoped allow-all on the required ports, and you tighten them through that
engine's native passthrough lists — `networkPolicy.<engine>.ingress` and
`networkPolicy.<engine>.egress` — which are merged verbatim into the rendered
policy's `spec`. For example, adding identity-based ingress and a `toFQDNs` egress
under the Cilium engine:

```yaml
networkPolicy:
  enabled: true
  engine: cilium
  cilium:
    ingress:
      - fromEndpoints:
          - matchLabels:
              app.kubernetes.io/name: flagger
    egress:
      - toFQDNs:
          - matchName: bucket.example.com
        toPorts:
          - ports:
              - port: "443"
                protocol: TCP
```

## Required traffic

The two tables below are the engine-agnostic matrix of what the controller pod must
be allowed to send and receive. Some peers are selectable by pod or namespace label;
others — the kube-apiserver and out-of-cluster endpoints — have no label to select
on and must be expressed as `ipBlock` CIDRs.

### Ingress

| Port | Source | Mode | Selectable by label? |
|---|---|---|---|
| Webhook (`ports.webhook`, `9443`) | The kube-apiserver, dialing the validating admission webhook | webhook | No — the apiserver is not a pod |
| Metrics (`ports.metrics`, `8080`) | Prometheus scraping `/metrics` | always | Yes — by the scraper's pod or namespace |
| Gate (`ports.gate`, `8082`) | Flagger, polling the read-only stage-gate endpoint for the promotion verdict | gate | Yes — by Flagger's namespace |

The metrics rule renders unconditionally, so the policy is never an empty-ingress
deny-all by accident. The webhook rule renders only when `webhook.enabled`, and the
gate rule only when `gate.enabled`.

The apiserver sources traffic from an address that is not a pod, so the webhook rule
cannot be narrowed with a `podSelector` or `namespaceSelector`. Leaving the webhook
`from` list empty keeps the port reachable, which is what lets the apiserver reach
the webhook. Authenticity on the webhook port is enforced by TLS and the CA bundle
on the `ValidatingWebhookConfiguration`, not by the network layer — see the
[admission webhook page](/usage/admission-webhook/).

### Egress

Egress only matters when you opt into it (`networkPolicy.egress.enabled`). The
controller needs the following outbound flows.

| Destination | Purpose | Selectable by label? |
|---|---|---|
| Cluster DNS | Name resolution — without it every other egress flow fails | Yes — by the DNS namespace |
| Local kube-apiserver | All controller reads and writes go through the apiserver | No — `ipBlock` CIDR only |
| Remote-cluster apiserver | A StageSet's `spec.kubeConfig` points the apply at another cluster's apiserver | No — `ipBlock` CIDR only |
| source-controller / producer | Artifact fetch from Flux's source-controller or a producer's namespace | Yes — `flux-system` / the producer namespace |
| S3 rollback store | The S3-compatible bucket backing the rollback store, when configured | Depends — in-cluster MinIO is label-selectable; an external bucket is `ipBlock` only |
| OTLP collector | Shipping traces when an OTLP endpoint is configured | Depends — in-cluster collector is label-selectable; an external one is `ipBlock` only |
| `http` action / direct `sourceRef` URLs | Targets reached by `http` actions and any direct `sourceRef` URL | No — `ipBlock` CIDR only |

The kube-apiserver is never label-selectable, so its egress rule must be an
`ipBlock` CIDR. The same applies to any remote-cluster apiserver, S3 bucket, OTLP
collector, or `http`-action target that lives outside the cluster. See
[multi-cluster](/usage/multi-cluster/), [rollback](/usage/rollback/),
[producer-aware sources](/usage/producer-aware-sources/), and
[actions](/usage/actions/) for the flows behind each row.

## Configuring ingress

Under the `kubernetes` engine, enable the policy and tighten each port through its
`from` knob. An empty `from` list leaves that port open; a non-empty list restricts
it to the listed peers.

```yaml
networkPolicy:
  enabled: true
  # The apiserver dials the webhook and has no label to select on — leave open.
  webhook:
    from: []
  # Scope metrics scraping to the monitoring namespace.
  metrics:
    from:
      - namespaceSelector:
          matchLabels:
            kubernetes.io/metadata.name: monitoring
  # Scope the gate to Flagger's namespace.
  gate:
    from:
      - namespaceSelector:
          matchLabels:
            kubernetes.io/metadata.name: flagger-system
```

Anything the per-port knobs do not cover goes into `additionalIngress`, which is
merged verbatim into the policy:

```yaml
networkPolicy:
  enabled: true
  additionalIngress:
    - ports:
        - protocol: TCP
          port: 8080
      from:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: monitoring
```

## Opt-in egress

Egress is off by default, and deliberately so. Adding the `Egress` policy type flips
the controller pod to default-deny for outbound traffic — everything not explicitly
allowed is dropped. Getting the allow-list complete is the cluster operator's risk,
because most of the controller's outbound peers — the kube-apiserver, any
remote-cluster apiserver, S3, the OTLP collector, `http`-action targets — have no
label to select on and so depend on `ipBlock` CIDRs that vary per cluster. An
incomplete list does not fail loudly; it silently cuts that path off.

> **Warning:** Enabling egress **without** an `ipBlock` for the kube-apiserver cuts
> the controller off from the cluster — it can no longer read or write any object,
> and every reconcile fails. Always include the apiserver CIDR before turning egress
> on.

Find the apiserver's address with:

```shell
kubectl get endpoints kubernetes -n default -o jsonpath='{.subsets[*].addresses[*].ip}'
```

Use that IP as a `/32` (or your control plane's CIDR for an HA apiserver). A
complete controller egress block — DNS, the apiserver, source-controller, S3, and an
OTLP collector — looks like this:

```yaml
networkPolicy:
  enabled: true
  egress:
    enabled: true
    # DNS to the cluster DNS namespace. Without this, every flow below
    # fails name resolution.
    dns: true
    dnsNamespace: kube-system
    to:
      # kube-apiserver — not label-selectable, so an ipBlock CIDR.
      # Replace with the IP(s) from the command above.
      - to:
          - ipBlock:
              cidr: 10.0.0.1/32
        ports:
          - protocol: TCP
            port: 443
      # source-controller — artifact fetch from the flux-system namespace.
      - to:
          - namespaceSelector:
              matchLabels:
                kubernetes.io/metadata.name: flux-system
      # S3 rollback store — an ipBlock CIDR for an external endpoint. For
      # in-cluster MinIO, use a namespaceSelector instead.
      - to:
          - ipBlock:
              cidr: 198.51.100.0/24
        ports:
          - protocol: TCP
            port: 443
      # OTLP collector — an ipBlock CIDR for an external endpoint. For an
      # in-cluster collector, use a namespaceSelector instead.
      - to:
          - ipBlock:
              cidr: 203.0.113.10/32
        ports:
          - protocol: TCP
            port: 4317
```

Add an `ipBlock` peer for each remote-cluster apiserver named by a `spec.kubeConfig`
and for each `http`-action / direct `sourceRef` host the controller reaches. Trim
the rest to what your install actually uses: drop the S3 block when no rollback store
is configured, and drop the OTLP block when tracing is off. The apiserver and DNS
rules are non-negotiable for the controller.

For the full set of chart values, see
[Helm chart values](/installation/helm-values/).
</content>
