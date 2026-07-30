// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"errors"
	"fmt"

	fluxmeta "github.com/fluxcd/pkg/apis/meta"
	authutils "github.com/fluxcd/pkg/auth/utils"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

// clusterTarget is a client plus the RESTMapper for the cluster it talks to.
// The mapper is part of the target because a remote cluster has its own API
// surface — reusing the controller cluster's mapper would mis-resolve GVKs.
type clusterTarget struct {
	client client.Client
	mapper apimeta.RESTMapper
	// token is the minted bearer token a local-cluster client was built with.
	// A cached local client is only reused while this matches the freshly
	// minted token — a stale BearerToken authenticates as nobody once it
	// expires, so the entry is rebuilt when the token rotates. Empty for
	// remote targets (which authenticate via the kubeconfig's own credentials)
	// and for the no-tenant default.
	token string
}

// remoteConfigBuilder turns a spec.kubeConfig reference into the rest.Config the
// apply runs through. Both reference shapes route through it:
//
//   - secretRef names a self-contained kubeconfig stored in a Secret; the config
//     carries its own embedded credentials.
//   - configMapRef selects cloud-provider workload-identity auth (AWS / Azure /
//     GCP / generic): the ConfigMap names the provider and cluster, and the
//     bearer token is minted by the cloud's IAM/STS on every request.
//
// The returned cacheKey participates in targetCluster's per-target cache so a
// rotated kubeconfig (or a different ConfigMap) builds a fresh connection while
// an unchanged one reuses the discovered RESTMapper. Production wires
// defaultRemoteConfigBuilder; tests inject a fake that points at envtest.
type remoteConfigBuilder interface {
	RESTConfig(ctx context.Context, kc *fluxmeta.KubeConfigReference, namespace, serviceAccount string) (cfg *rest.Config, cacheKey string, err error)
}

// errInvalidKubeConfigSpec marks a kubeConfig failure that no retry can fix —
// a malformed configMapRef (unknown provider, missing required keys) or an
// unparseable kubeconfig Secret. It is terminal: the reconciler surfaces it as
// ReasonInvalidSpec rather than retrying on every reconcile.
var errInvalidKubeConfigSpec = errors.New("invalid spec.kubeConfig")

// defaultRemoteConfigBuilder is the production remoteConfigBuilder. Both paths
// read their in-cluster object (the Secret / ConfigMap) as the StageSet's
// serviceAccountName: the reference is spec-supplied, so a read on the
// controller's identity would let a StageSet author point at a kubeconfig their
// own ServiceAccount cannot see and have the controller connect with it.
type defaultRemoteConfigBuilder struct {
	r *StageSetReconciler
}

// RESTConfig builds a rest.Config for the kubeConfig reference. secretRef parses
// a stored kubeconfig; configMapRef hands off to fluxcd/pkg/auth, which reads
// the ConfigMap, validates its provider/keys, and returns a config whose bearer
// token is re-minted from the cloud STS per request.
//
// The tenant client for the LOCAL cluster is resolved here, and the recursion
// terminates because that call passes kc=nil: resolveTarget's remote branch (the
// only caller of this method) is not re-entered. It must also stay outside
// resolveTarget's tenantMu — the nested call takes that lock itself, and a Go
// mutex is not reentrant. resolveTarget calls this before locking, which is what
// makes the ordering safe.
func (b defaultRemoteConfigBuilder) RESTConfig(ctx context.Context, kc *fluxmeta.KubeConfigReference, namespace, serviceAccount string) (*rest.Config, string, error) {
	tenant, _, err := b.r.targetCluster(ctx, namespace, serviceAccount, nil)
	if err != nil {
		return nil, "", fmt.Errorf("kubeConfig: %w", err)
	}
	switch {
	case kc.SecretRef != nil:
		raw, version, err := b.r.kubeconfigBytes(ctx, tenant, namespace, kc.SecretRef)
		if err != nil {
			return nil, "", err
		}
		// A malformed kubeconfig Secret is treated as a transient stage failure
		// (not errInvalidKubeConfigSpec) so the secretRef path keeps its existing
		// behavior: the run fails the stage and backs off rather than going
		// terminal.
		cfg, err := clientcmd.RESTConfigFromKubeConfig(raw)
		if err != nil {
			return nil, "", fmt.Errorf("parsing kubeConfig secret %q: %w", kc.SecretRef.Name, err)
		}
		return cfg, "secret/" + namespace + "/" + kc.SecretRef.Name + "/" + version, nil
	case kc.ConfigMapRef != nil:
		// fluxcd/pkg/auth reads the ConfigMap, dispatches on its "provider" key
		// (aws|azure|gcp|generic), validates the per-provider inputs, and mints
		// the cluster bearer token from the cloud's IAM/STS. A bad provider name
		// or missing required key is terminal — wrap it as such.
		//
		// The cache key folds in the ConfigMap's resourceVersion so an in-place
		// edit (a changed provider/cluster/region) rebuilds the connection
		// instead of being masked until token rotation or a restart — mirroring
		// the secretRef path. A missing ConfigMap is terminal: no retry brings it
		// back without a spec/object change.
		version, err := b.r.configMapResourceVersion(ctx, tenant, namespace, kc.ConfigMapRef.Name)
		if err != nil {
			return nil, "", fmt.Errorf("%w: cloud-provider kubeConfig configMap %q: %w", errInvalidKubeConfigSpec, kc.ConfigMapRef.Name, err)
		}
		// fluxcd/pkg/auth re-reads the ConfigMap through the client it is given,
		// so it takes the tenant's too — otherwise the check above would be
		// bounded by the tenant while the read that matters was not.
		cfg, err := authutils.GetRESTConfig(ctx, *kc, namespace, tenant)
		if err != nil {
			return nil, "", fmt.Errorf("%w: cloud-provider kubeConfig configMap %q: %w", errInvalidKubeConfigSpec, kc.ConfigMapRef.Name, err)
		}
		return cfg, "configmap/" + namespace + "/" + kc.ConfigMapRef.Name + "/" + version, nil
	default:
		return nil, "", fmt.Errorf("%w: spec.kubeConfig sets neither secretRef nor configMapRef", errInvalidKubeConfigSpec)
	}
}

// targetCluster returns the client and mapper every cluster write of a run is
// performed through. Two orthogonal modifiers compose onto the controller's own
// connection:
//
//   - spec.serviceAccountName makes the apply assume that SA's identity. On the
//     local (controller) cluster this is done by minting a short-lived
//     TokenRequest bearer token for system:serviceaccount:<ns>:<sa> and
//     authenticating with it — least privilege, no `impersonate` verb on the
//     controller. On a remote cluster (spec.kubeConfig) the controller has no
//     authority to mint and a controller-cluster token would be rejected by the
//     remote apiserver (wrong issuer/audience), so the remote path keeps
//     header impersonation against the kubeconfig's credentials.
//   - spec.kubeConfig retargets the apply to a remote cluster. secretRef builds
//     it from a kubeconfig stored in a Secret in the StageSet's namespace;
//     configMapRef selects cloud-provider workload-identity auth (the four
//     fluxcd/pkg/auth providers), built from a ConfigMap in the same namespace.
//
// With neither set, the controller's own client and mapper are returned — the
// single-cluster, single-tenant default, no extra objects built. Bookkeeping
// the target must not see (StageInventory shards, StageSet status) always stays
// on the controller client; only apply/prune/verify/actions use this target.
//
// Results are cached: local clients per <namespace>/<sa> (rebuilt when the
// minted token rotates so a cached client never carries an expired token),
// remote clients per <namespace>/<sa>/<kubeConfig-identity> so a rotated
// kubeconfig builds a fresh connection while an unchanged one reuses the
// discovered RESTMapper.
//
// A minted-token target is handed back wrapped in retryUnauthorizedClient: the
// token is bound to the ServiceAccount's UID while the cache is keyed by its
// name, so a recreated ServiceAccount leaves a cached credential the apiserver
// refuses. The wrapper turns each such 401 into an eviction plus one retry
// instead of an hour of failing calls.
func (r *StageSetReconciler) targetCluster(ctx context.Context, ns, sa string, kc *fluxmeta.KubeConfigReference) (client.Client, apimeta.RESTMapper, error) {
	target, mapper, err := r.resolveTarget(ctx, ns, sa, kc)
	if err != nil {
		return nil, nil, err
	}
	if !r.mintsTokenFor(sa, kc) {
		return target, mapper, nil
	}
	return newRetryUnauthorizedClient(target, ns, sa, func(ctx context.Context) (client.Client, error) {
		r.forgetTenant(ns, sa)
		fresh, _, err := r.resolveTarget(ctx, ns, sa, nil)
		return fresh, err
	}), mapper, nil
}

// mintsTokenFor reports whether a target for sa authenticates with a token this
// controller minted — the only case where evicting the cached credential and
// re-minting can turn a 401 into a success. The controller's own client (no
// tenant SA, or the envtest SkipImpersonation path) carries no minted
// credential, and a remote spec.kubeConfig target authenticates with the
// kubeconfig's own.
func (r *StageSetReconciler) mintsTokenFor(sa string, kc *fluxmeta.KubeConfigReference) bool {
	return kc == nil && sa != "" && !r.SkipImpersonation
}

// localTargetKey is the targets-map key for a local-cluster tenant connection.
// forgetTenant and resolveTarget must agree on it, so it lives in one place.
func localTargetKey(ns, sa string) string { return "local/" + ns + "/" + sa }

// forgetTenant drops the cached credential for ns/sa and the connection built
// around it, so the next resolve mints a fresh token and dials with it. The
// token eviction alone would suffice — a targets entry is only reused while its
// baked-in token matches the freshly minted one — but dropping the entry too
// keeps the map bounded by the tenants that are actually live rather than by
// every tenant the process has ever seen.
func (r *StageSetReconciler) forgetTenant(ns, sa string) {
	r.tokens.Forget(ns, sa)
	r.tenantMu.Lock()
	defer r.tenantMu.Unlock()
	delete(r.targets, localTargetKey(ns, sa))
}

// forgetDeletedTenants evicts the credentials and connections a StageSet on its
// way out was the last user of. targets holds one live transport per (namespace,
// ServiceAccount) and tokens a credential for the same key, and neither is
// bounded by anything but process lifetime — a cluster whose namespaces come and
// go would otherwise accumulate a dead connection per tenant it ever applied
// for.
//
// Another live StageSet in the namespace using the same ServiceAccount keeps the
// entry: evicting it would only force that StageSet to re-mint on its next
// reconcile. A List failure skips eviction entirely — an entry outliving its
// tenant costs memory, nothing more, and the token's own TTL bounds the
// credential regardless.
func (r *StageSetReconciler) forgetDeletedTenants(ctx context.Context, ss *stagesv1.StageSet) {
	used := map[string]struct{}{}
	if sa := ss.Spec.ServiceAccountName; sa != "" {
		used[sa] = struct{}{}
	}
	for i := range ss.Spec.Stages {
		if sa := effectiveServiceAccount(ss, &ss.Spec.Stages[i]); sa != "" {
			used[sa] = struct{}{}
		}
	}
	if len(used) == 0 {
		return
	}
	var others stagesv1.StageSetList
	if err := r.List(ctx, &others, client.InNamespace(ss.Namespace)); err != nil {
		log.FromContext(ctx).V(1).Info("skipping tenant credential eviction: listing StageSets failed", "error", err)
		return
	}
	for i := range others.Items {
		other := &others.Items[i]
		if other.Name == ss.Name || !other.DeletionTimestamp.IsZero() {
			continue
		}
		delete(used, other.Spec.ServiceAccountName)
		for j := range other.Spec.Stages {
			delete(used, effectiveServiceAccount(other, &other.Spec.Stages[j]))
		}
	}
	for sa := range used {
		r.forgetTenant(ss.Namespace, sa)
	}
}

// resolveTarget is targetCluster without the 401-retry wrapper: it builds (or
// returns the cached) raw connection. The wrapper's refresh calls back into it,
// so keeping the two apart is what stops a refreshed client from being wrapped
// a second time on every eviction.
func (r *StageSetReconciler) resolveTarget(ctx context.Context, ns, sa string, kc *fluxmeta.KubeConfigReference) (client.Client, apimeta.RESTMapper, error) {
	if kc == nil && sa == "" {
		return r.Client, r.RESTMapper, nil
	}

	remote := kc != nil

	// Local cluster + a tenant SA, impersonation disabled: apply under the
	// controller's own identity without minting. Only the envtest harness sets
	// SkipImpersonation; production keeps it false so the tenant SA's RBAC bounds
	// the apply.
	if !remote && sa != "" && r.SkipImpersonation {
		return r.Client, r.RESTMapper, nil
	}

	var (
		cfg        *rest.Config
		baseMapper apimeta.RESTMapper
		key        string
		token      string
	)
	if remote {
		builder := r.remoteConfig
		if builder == nil {
			builder = defaultRemoteConfigBuilder{r: r}
		}
		var (
			ckey string
			err  error
		)
		cfg, ckey, err = builder.RESTConfig(ctx, kc, ns, sa)
		if err != nil {
			return nil, nil, err
		}
		key = "remote/" + ns + "/" + ckey + "/" + sa
		// Remote: header impersonation against the kubeconfig's credentials. A
		// token minted on the controller cluster is not valid here.
		if sa != "" {
			cfg.Impersonate = rest.ImpersonationConfig{
				UserName: fmt.Sprintf("system:serviceaccount:%s:%s", ns, sa),
			}
		}
	} else {
		if r.Config == nil {
			return nil, nil, fmt.Errorf("spec.serviceAccountName %q set but the controller has no rest config to mint a token with", sa)
		}
		if r.tokens == nil {
			return nil, nil, fmt.Errorf("spec.serviceAccountName %q set but token minting is not configured", sa)
		}
		// Local: mint a bearer token for the tenant SA and authenticate as it.
		// The minted token is the ONLY credential on this config — clearing
		// Impersonate, BearerTokenFile, and the operator's own static auth so a
		// compromised StageSet can't reach beyond the tenant SA's RBAC.
		var err error
		token, err = r.tokens.Token(ctx, ns, sa)
		if err != nil {
			return nil, nil, fmt.Errorf("minting token for %s/%s: %w", ns, sa, err)
		}
		cfg = tenantRestConfig(r.Config, token)
		baseMapper = r.RESTMapper // controller cluster: reuse the manager's mapper
		key = localTargetKey(ns, sa)
	}

	r.tenantMu.Lock()
	defer r.tenantMu.Unlock()
	if t, ok := r.targets[key]; ok && t.token == token {
		// A cached local client is only reused while its baked-in token still
		// matches the freshly minted one; on rotation the entry below replaces it.
		return t.client, t.mapper, nil
	}

	mapper := baseMapper
	if mapper == nil { // remote cluster: discover its API surface once
		httpClient, err := rest.HTTPClientFor(cfg)
		if err != nil {
			return nil, nil, fmt.Errorf("http client for target cluster: %w", err)
		}
		mapper, err = apiutil.NewDynamicRESTMapper(cfg, httpClient)
		if err != nil {
			return nil, nil, fmt.Errorf("REST mapper for target cluster: %w", err)
		}
	}
	c, err := client.New(cfg, client.Options{Scheme: r.Client.Scheme(), Mapper: mapper})
	if err != nil {
		return nil, nil, fmt.Errorf("building target client for %q: %w", key, err)
	}

	if r.targets == nil {
		r.targets = map[string]clusterTarget{}
	}
	r.targets[key] = clusterTarget{client: c, mapper: mapper, token: token}
	return c, mapper, nil
}

// gateReader picks the reader the promotion restart/event gates use for their
// pod/event reads. When targetCluster returned the controller's own cached client
// (the local single-tenant and skip-impersonation paths), reading through it
// would back the List with a cluster-wide informer; the uncached APIReader avoids
// that. Impersonated and remote targets are already uncached (client.New), so
// they read through the target directly. A nil APIReader (tests) falls back to
// the target.
func (r *StageSetReconciler) gateReader(target client.Client) client.Reader {
	if target == r.Client && r.APIReader != nil {
		return r.APIReader
	}
	return target
}

// tenantRestConfig assembles the rest.Config for a local-cluster tenant client:
// a clone of the controller's connection whose ONLY credential is the minted
// bearer token. Stripping Impersonate, BearerTokenFile, and the controller's
// own static auth leaves the token as the sole authenticator, so the tenant
// SA's RBAC is the ceiling. The controller's TLS trust (CA + ServerName) is
// preserved and Insecure is forced off — a dev kubeconfig's
// insecure-skip-tls-verify must not flow into tenant API calls.
func tenantRestConfig(base *rest.Config, token string) *rest.Config {
	cfg := rest.AnonymousClientConfig(base)
	cfg.BearerToken = token
	cfg.BearerTokenFile = ""
	cfg.Impersonate = rest.ImpersonationConfig{}
	cfg.TLSClientConfig.CAData = base.CAData
	cfg.TLSClientConfig.CAFile = base.CAFile
	cfg.TLSClientConfig.ServerName = base.ServerName
	cfg.TLSClientConfig.Insecure = false
	return cfg
}

// kubeconfigBytes reads the kubeconfig payload (and its resourceVersion, for
// cache invalidation) from a Secret in ns, through the supplied reader — the
// StageSet's tenant client. The key defaults to "value" per the Flux convention.
func (r *StageSetReconciler) kubeconfigBytes(ctx context.Context, reader client.Reader, ns string, ref *fluxmeta.SecretKeyReference) ([]byte, string, error) {
	var sec corev1.Secret
	if err := reader.Get(ctx, types.NamespacedName{Namespace: ns, Name: ref.Name}, &sec); err != nil {
		return nil, "", fmt.Errorf("kubeConfig secret %q: %w", ref.Name, err)
	}
	key := ref.Key
	if key == "" {
		key = "value"
	}
	data, ok := sec.Data[key]
	if !ok || len(data) == 0 {
		return nil, "", fmt.Errorf("kubeConfig secret %q has no non-empty key %q", ref.Name, key)
	}
	return data, sec.ResourceVersion, nil
}

// configMapResourceVersion reads the configMapRef ConfigMap's resourceVersion so
// the target-cluster cache key tracks in-place edits. It reads through the
// supplied reader — the StageSet's tenant client — matching the read
// fluxcd/pkg/auth then performs on the same object.
func (r *StageSetReconciler) configMapResourceVersion(ctx context.Context, reader client.Reader, ns, name string) (string, error) {
	var cm corev1.ConfigMap
	if err := reader.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &cm); err != nil {
		return "", fmt.Errorf("kubeConfig configMap %q: %w", name, err)
	}
	return cm.ResourceVersion, nil
}
