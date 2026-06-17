// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"fmt"

	fluxmeta "github.com/fluxcd/pkg/apis/meta"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
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
//   - spec.kubeConfig.secretRef retargets the apply to a remote cluster, built
//     from a kubeconfig stored in a Secret in the StageSet's namespace.
//
// With neither set, the controller's own client and mapper are returned — the
// single-cluster, single-tenant default, no extra objects built. Bookkeeping
// the target must not see (StageInventory shards, StageSet status) always stays
// on the controller client; only apply/prune/verify/actions use this target.
//
// Results are cached: local clients per <namespace>/<sa> (rebuilt when the
// minted token rotates so a cached client never carries an expired token),
// remote clients per <namespace>/<sa>/<secret>/<resourceVersion> so a rotated
// kubeconfig builds a fresh connection while an unchanged one reuses the
// discovered RESTMapper.
func (r *StageSetReconciler) targetCluster(ctx context.Context, ns, sa string, kc *fluxmeta.KubeConfigReference) (client.Client, apimeta.RESTMapper, error) {
	if kc == nil && sa == "" {
		return r.Client, r.RESTMapper, nil
	}
	if kc != nil && kc.ConfigMapRef != nil {
		return nil, nil, fmt.Errorf("spec.kubeConfig.configMapRef (cloud-provider auth) is not yet supported; use secretRef with a self-contained kubeconfig")
	}

	remote := kc != nil && kc.SecretRef != nil

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
		raw, version, err := r.kubeconfigBytes(ctx, ns, kc.SecretRef)
		if err != nil {
			return nil, nil, err
		}
		cfg, err = clientcmd.RESTConfigFromKubeConfig(raw)
		if err != nil {
			return nil, nil, fmt.Errorf("parsing kubeConfig secret %q: %w", kc.SecretRef.Name, err)
		}
		key = "remote/" + ns + "/" + kc.SecretRef.Name + "/" + version + "/" + sa
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
		key = "local/" + ns + "/" + sa
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
// cache invalidation) from a Secret in ns. The Secret is read with the
// controller's own client — connecting to the target cluster is the
// controller's job, not the tenant's — and the key defaults to "value" per the
// Flux convention.
func (r *StageSetReconciler) kubeconfigBytes(ctx context.Context, ns string, ref *fluxmeta.SecretKeyReference) ([]byte, string, error) {
	var sec corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: ref.Name}, &sec); err != nil {
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
