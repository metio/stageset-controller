---
title: Multi-cluster and tenancy
description: Impersonation, watch scoping, single-tenant cluster-admin, and remote clusters.
tags: [multi-cluster, tenancy, impersonation, rbac]
---

There are two ways to run the controller, and they map onto two different trust
models. Pick the one that matches your cluster:

- **Multi-tenant** — the controller holds no write access of its own and applies
  every `StageSet` impersonating that `StageSet`'s `serviceAccountName`. Each
  tenant's RBAC bounds what its releases can touch. This is the chart default.
- **Single-tenant** — the cluster has one operator, so per-tenant isolation buys
  nothing. Run the controller under its own identity bound to `cluster-admin` and
  skip impersonation entirely — the model Flux's `helm-controller` uses in its
  default install.

The two sections below set each one up. The optional
[watch scoping](#scoping-the-controller-to-a-namespace-set) narrows *which*
namespaces a multi-tenant controller sees.

## Impersonation (multi-tenant)

The controller never applies your manifests as itself. Set `serviceAccountName`
and every operation for that `StageSet` — build, apply, prune, actions — is
performed impersonating that ServiceAccount. The `StageSet` can do exactly what the
SA's RBAC permits, and nothing more.

```yaml
spec:
  serviceAccountName: payments-deployer    # all writes impersonate this SA
  stages:
    - name: app
      sourceRef:
        name: payments-app
```

Grant the SA only the rights that release needs:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: payments-deployer
  namespace: payments
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: edit
subjects:
  - kind: ServiceAccount
    name: payments-deployer
    namespace: payments
```

This is the multi-tenancy model: isolation comes from each `StageSet` being bounded
by its tenant SA, not from the controller's own grant — by default the chart gives
the controller `impersonate` and read access, no blanket write. A `StageSet` with no
`serviceAccountName`, or one bound to a too-narrow SA, fails closed rather than
escalating.

## Single-tenant cluster-admin

On a cluster with a single operator, per-`StageSet` impersonation is friction with
no payoff — there is no other tenant to isolate from. Run the controller the way
Flux's `helm-controller` runs by default: under its own ServiceAccount, bound to
the built-in `cluster-admin` ClusterRole. `StageSet`s then omit `serviceAccountName`
and apply as the controller, which can write any kind cluster-wide.

Turn it on with one Helm value:

```yaml
rbac:
  clusterAdmin: true     # bind the controller SA to cluster-admin
```

```bash
helm upgrade --install stageset-controller \
  oci://ghcr.io/metio/helm-charts/stageset-controller \
  -n stageset-system --create-namespace \
  --set rbac.clusterAdmin=true
```

`StageSet`s then need nothing tenancy-related — they apply directly:

```yaml
apiVersion: stages.metio.wtf/v1
kind: StageSet
metadata:
  name: platform
  namespace: stageset-system
spec:
  stages:
    - name: app
      sourceRef:
        name: platform-app    # applied by the controller's cluster-admin identity
```

When `serviceAccountName` is unset and no `kubeConfig` is given, the controller
applies with its own client — so the `cluster-admin` binding is what lets those
`StageSet`s write. The trade-off: every `StageSet` on the cluster has full write
access, so this is for single-tenant clusters only. Leave `rbac.clusterAdmin` at its
default `false` and use [impersonation](#impersonation-multi-tenant) whenever more
than one team shares the cluster. The two mix — a cluster-admin controller still
honors `serviceAccountName` on any `StageSet` that sets it, dropping to that SA's
rights for that release.

## Scoping the controller to a namespace set

By default the controller watches every namespace. To run one controller per
tenant-group instead — disjoint deployments that each see only their own
namespaces — set `controller.watchNamespaces`:

```yaml
controller:
  watchNamespaces:
    - team-a
    - team-b
```

This does two things together:

- **Cache scoping.** The manager's informers only observe `StageSet`s and sources
  in the listed namespaces. Resources elsewhere never enter the cache, so the
  controller cannot act on them even if RBAC would allow it.
- **RBAC pivot.** The chart stops binding the tenant ClusterRole cluster-wide and
  instead renders one `RoleBinding` per listed namespace — defense in depth, so the
  apiserver also refuses out-of-scope calls. (The cluster-scoped webhook-caBundle
  grant stays a `ClusterRoleBinding`, since a `ValidatingWebhookConfiguration` is
  not namespaced.)

Run several releases with disjoint `watchNamespaces` lists to shard the cluster
across independent controller instances. Combine it with impersonation for the
tightest setup: each instance sees only its namespaces, and each `StageSet` is
bounded by its tenant SA.

## Remote clusters

Point a `StageSet` at another cluster with `kubeConfig`, referencing a Secret that
holds a kubeconfig. Combined with `serviceAccountName`, the controller applies to
the remote cluster as the impersonated identity there.

```yaml
spec:
  serviceAccountName: payments-deployer
  kubeConfig:
    secretRef:
      name: prod-eu-kubeconfig
      # key defaults to "value" (the Flux convention); set it to override
  stages:
    - name: app
      sourceRef:
        name: payments-app
```

The Secret is read with the controller's own identity — connecting to the target
cluster is the controller's job — and the kubeconfig payload defaults to the
`value` key. A self-contained kubeconfig is required; `configMapRef`-style
cloud-provider auth is not supported.

Cross-namespace `sourceRef` and `dependsOn` references can be disabled
cluster-wide with the controller's `--no-cross-namespace-refs` flag when you want
hard namespace isolation.
