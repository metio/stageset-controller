# Per-StageSet impersonation and remote-cluster apply

## Decision

A run's cluster writes go through a **target connection** assembled from two orthogonal, composable modifiers on the controller's own client:

- `spec.serviceAccountName` impersonates that ServiceAccount via kustomize-controller-style header impersonation (`Impersonate-User: system:serviceaccount:<ns>:<sa>`).
- `spec.kubeConfig.secretRef` retargets the apply to a remote cluster, built from a self-contained kubeconfig in a Secret.

With neither set, the controller's own client and RESTMapper are used — the single-cluster, single-tenant default. Bookkeeping the tenant must not see (StageInventory shards, StageSet status) always stays on the controller's own cluster and identity; only apply, prune, health-check reads, and the typed actions use the target.

## Context and alternatives

**Impersonation vs token minting.** The sibling JaaS operator mints ServiceAccount tokens via the TokenRequest API to avoid granting the controller the `impersonate` verb. We chose header impersonation instead because the design target is **kustomize-controller parity** — platform RBAC, `--default-service-account` conventions, and operator mental models transfer unchanged. The cost is one `impersonate` grant on `serviceaccounts` in the controller's ClusterRole; the benefit is that a run reaches no further than the named SA's RBAC, enforced by the apiserver, with no token lifecycle to manage. Only the SA username is impersonated, not its groups: tenant RoleBindings bind the SA subject directly, which RBAC matches by username.

**secretRef vs configMapRef for kubeConfig.** `meta.KubeConfigReference` also supports a cloud-provider `configMapRef` (AWS/Azure/GCP auth). We support only the generic `secretRef` (a self-contained kubeconfig) and reject `configMapRef` with a clear error rather than silently ignoring it. Cloud-provider auth pulls in provider SDKs and credential plumbing that deserve their own decision; deferring keeps the first cut dependency-light and unambiguous.

**Why the remote path needs its own RESTMapper.** A remote cluster has a different API surface, so the target carries a dynamic RESTMapper discovered from its kubeconfig — reusing the controller cluster's mapper would mis-resolve GVKs. StageInventory stays local precisely so the inventory CRD is never a remote-cluster dependency.

The two modifiers compose: a run can target a remote cluster *and* impersonate an SA there. Connections are cached per `<namespace>/<sa>` (local) or `<namespace>/<secret>/<resourceVersion>/<sa>` (remote), so a rotated kubeconfig rebuilds while an unchanged one reuses the discovered mapper.

See [`../content/design/stageset.md`](../content/design/stageset.md) (Multi-tenancy, Security) for the full mechanism.
