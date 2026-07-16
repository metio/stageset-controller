---
title: Multi-cluster and tenancy
description: Impersonation, watch scoping, single-tenant cluster-admin, and remote clusters.
tags: [multi-cluster, tenancy, impersonation, rbac]
---

There are two ways to run the controller, and they map onto two different trust
models. Pick the one that matches your cluster:

- **Multi-tenant** — the controller holds no write access of its own and applies
  every `StageSet` as that `StageSet`'s `serviceAccountName`. Each tenant's RBAC
  bounds what its releases can touch. This is the chart default.
- **Single-tenant** — the cluster has one operator, so per-tenant isolation buys
  nothing. Run the controller under its own identity bound to `cluster-admin` and
  skip impersonation entirely — the model Flux's `helm-controller` uses in its
  default install.

The two sections below set each one up. The optional
[watch scoping](#scoping-the-controller-to-a-namespace-set) narrows *which*
namespaces a multi-tenant controller sees.

## Tenant ServiceAccounts (multi-tenant)

The controller never applies your manifests as itself. Set `serviceAccountName`
and every operation for that `StageSet` — build, apply, prune, actions — runs as
that ServiceAccount. The `StageSet` can do exactly what the SA's RBAC permits, and
nothing more.

On the local cluster the controller assumes the tenant identity by minting a
short-lived [TokenRequest](https://kubernetes.io/docs/reference/kubernetes-api/authentication-resources/token-request-v1/)
token for the named ServiceAccount and authenticating as it. That needs only
`create` on `serviceaccounts/token` in the controller's ClusterRole — the powerful
`impersonate` verb is not granted. The token is cached per ServiceAccount and
re-minted as it nears expiry.

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
the controller `create` on `serviceaccounts/token` and read access, no blanket write
and no `impersonate`. A `StageSet` with no `serviceAccountName`, or one bound to a
too-narrow SA, fails closed rather than escalating.

### Per-stage ServiceAccounts

A stage can set its own `serviceAccountName`, overriding the StageSet default for
that stage alone. Every cluster operation the stage performs — apply, prune,
readiness verification, actions, and rollback — runs as the stage's ServiceAccount,
bounded by its RBAC. This lets one ordered `StageSet` promote a change through
environments that each have their own identity, without splitting the rollout
across separate `StageSet`s:

```yaml
spec:
  serviceAccountName: staging-deployer      # default for stages that omit it
  stages:
    - name: staging
      sourceRef: { name: payments-app }     # runs as staging-deployer
    - name: production
      serviceAccountName: prod-deployer      # runs as prod-deployer
      sourceRef: { name: payments-app }
```

Each stage's identity is minted the same way as the StageSet default — a
short-lived TokenRequest token locally, or header impersonation against a remote
`kubeConfig`. Stages sharing a ServiceAccount share one cached token. SOPS
decryption keys are read under the StageSet-level `serviceAccountName`, since
`spec.decryption` is a StageSet-wide setting rather than a per-stage one.

## Single-tenant cluster-admin

On a cluster with a single operator, per-`StageSet` tenant identities are friction
with no payoff — there is no other tenant to isolate from. Run the controller the way
Flux's `helm-controller` runs by default: under its own ServiceAccount, bound to
the built-in `cluster-admin` ClusterRole. `StageSet`s then omit `serviceAccountName`
and apply as the controller, which can write any kind cluster-wide.

Turn it on with one Helm value:

```yaml
rbac:
  clusterAdmin: true     # bind the controller SA to cluster-admin
```

```bash
helm --namespace stageset-system upgrade --install stageset-controller \
  oci://ghcr.io/metio/helm-charts/stageset-controller \
  --create-namespace \
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
default `false` and give each `StageSet` a [tenant ServiceAccount](#tenant-serviceaccounts-multi-tenant)
whenever more than one team shares the cluster. The two mix — a cluster-admin controller
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
holds a kubeconfig. The controller authenticates to the remote cluster with the
identity in that kubeconfig — token minting is local-cluster only, since a token
minted on the controller's cluster carries the wrong issuer and audience for a
remote apiserver. When `serviceAccountName` is also set, the controller layers
classic header impersonation onto the kubeconfig identity, so the remote apply runs
as that ServiceAccount on the remote cluster.

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

The Secret is read as the StageSet's `spec.serviceAccountName`, in the StageSet's
own namespace, so grant that ServiceAccount `get` on it — a kubeconfig the
ServiceAccount cannot read fails the stage rather than connecting. The kubeconfig
payload defaults to the `value` key.

### Cloud-provider auth

Instead of storing a self-contained kubeconfig in a Secret, point `kubeConfig` at
a `configMapRef` to authenticate through a cloud provider's workload identity. The
cluster's API server address and CA come from the ConfigMap, and the bearer token
is minted by the provider's IAM/STS on each request:

```yaml
spec:
  kubeConfig:
    configMapRef:
      name: prod-eks-auth
  stages:
    - name: app
      sourceRef:
        name: payments-app
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: prod-eks-auth
data:
  provider: aws              # one of: aws, azure, gcp, generic (required)
  cluster: prod-eks          # provider-specific cluster resource name
  address: https://XXXX.eks.amazonaws.com  # API server address
  ca.crt: |                  # PEM-encoded CA bundle (optional)
    -----BEGIN CERTIFICATE-----
    ...
    -----END CERTIFICATE-----
  audiences: |               # OIDC audiences for the SA token (optional)
    sts.amazonaws.com
  serviceAccountName: deployer  # cloud identity to impersonate (optional)
```

The ConfigMap is read as the StageSet's `spec.serviceAccountName`, like the
`secretRef` payload above, so that ServiceAccount needs `get` on it. Which keys
are required depends on the provider; the `provider` key is always required and
must be one of
`aws`, `azure`, `gcp`, or `generic`. When `serviceAccountName` is set in the
ConfigMap, that ServiceAccount (in the StageSet's namespace) is the cloud identity
whose workload-identity binding the provider exchanges for a cluster token; when
absent, the controller's own ambient cloud credentials are used.

**Trust model:** the ConfigMap names the provider and the cluster, but the actual
cloud credential is never in the cluster — it is minted on demand from the cloud's
IAM/STS using the controller's (or the named ServiceAccount's) workload identity.
A malformed ConfigMap (unknown `provider`, missing a required key, or a missing
ConfigMap) fails the StageSet terminally with `reason: InvalidSpec`; admission
only checks that `configMapRef.name` is set, because the ConfigMap contents are
not visible to the webhook.

`secretRef` and `configMapRef` are mutually exclusive — set exactly one.

Cross-namespace `sourceRef` and `dependsOn` references can be disabled
cluster-wide with the controller's `--no-cross-namespace-refs` flag when you want
hard namespace isolation.
