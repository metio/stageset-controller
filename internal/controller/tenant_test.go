// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	fluxmeta "github.com/fluxcd/pkg/apis/meta"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

// builderScheme registers core types (Secret, ConfigMap) plus the StageSet API
// so the fake client used to exercise defaultRemoteConfigBuilder can read them.
func builderScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := scheme.AddToScheme(s); err != nil {
		t.Fatalf("core AddToScheme: %v", err)
	}
	if err := stagesv1.AddToScheme(s); err != nil {
		t.Fatalf("stages AddToScheme: %v", err)
	}
	return s
}

func builderWith(t *testing.T, objs ...client.Object) *StageSetReconciler {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(builderScheme(t)).WithObjects(objs...).Build()
	return &StageSetReconciler{Client: c}
}

// A self-contained secretRef kubeconfig parses and yields a rest.Config plus a
// cache key that embeds the Secret name. No network, no cloud.
func TestDefaultRemoteConfigBuilder_SecretRef_OK(t *testing.T) {
	t.Parallel()
	const ns = "team-a"
	raw := []byte("apiVersion: v1\nkind: Config\nclusters:\n- name: c\n  cluster:\n    server: https://example.test:6443\ncontexts:\n- name: c\n  context:\n    cluster: c\n    user: u\ncurrent-context: c\nusers:\n- name: u\n  user:\n    token: abc\n")
	r := builderWith(t, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "kc"},
		Data:       map[string][]byte{"value": raw},
	})
	b := defaultRemoteConfigBuilder{r: r}
	cfg, key, err := b.RESTConfig(context.Background(), &fluxmeta.KubeConfigReference{
		SecretRef: &fluxmeta.SecretKeyReference{Name: "kc"},
	}, ns)
	if err != nil {
		t.Fatalf("RESTConfig err = %v, want nil", err)
	}
	if cfg.Host != "https://example.test:6443" {
		t.Fatalf("Host = %q, want the kubeconfig server", cfg.Host)
	}
	if !strings.Contains(key, "kc") || !strings.HasPrefix(key, "secret/") {
		t.Fatalf("cache key = %q, want a secret/...kc key", key)
	}
}

// A Secret whose payload is not a kubeconfig fails — but NOT as a terminal
// errInvalidKubeConfigSpec: the secretRef path keeps its existing behavior of
// failing the stage and backing off.
func TestDefaultRemoteConfigBuilder_SecretRef_BadKubeconfig(t *testing.T) {
	t.Parallel()
	const ns = "team-a"
	r := builderWith(t, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "kc"},
		Data:       map[string][]byte{"value": []byte("not a kubeconfig")},
	})
	b := defaultRemoteConfigBuilder{r: r}
	_, _, err := b.RESTConfig(context.Background(), &fluxmeta.KubeConfigReference{
		SecretRef: &fluxmeta.SecretKeyReference{Name: "kc"},
	}, ns)
	if err == nil {
		t.Fatal("RESTConfig err = nil, want a parse error")
	}
	if errors.Is(err, errInvalidKubeConfigSpec) {
		t.Fatalf("err = %v, secretRef parse failures must not be terminal InvalidSpec", err)
	}
}

// A configMapRef whose ConfigMap names a provider that is not one of
// aws/azure/gcp/generic is a terminal spec error — fluxcd/pkg/auth's
// ProviderByName rejects it. Real provider dispatch, no cloud account.
func TestDefaultRemoteConfigBuilder_ConfigMapRef_UnknownProvider(t *testing.T) {
	t.Parallel()
	const ns = "team-a"
	r := builderWith(t, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "cloud"},
		Data:       map[string]string{"provider": "totally-not-a-cloud"},
	})
	b := defaultRemoteConfigBuilder{r: r}
	_, _, err := b.RESTConfig(context.Background(), &fluxmeta.KubeConfigReference{
		ConfigMapRef: &fluxmeta.LocalObjectReference{Name: "cloud"},
	}, ns)
	if !errors.Is(err, errInvalidKubeConfigSpec) {
		t.Fatalf("err = %v, want errInvalidKubeConfigSpec", err)
	}
	if !strings.Contains(err.Error(), "totally-not-a-cloud") {
		t.Fatalf("err = %v, want it to name the bad provider", err)
	}
}

// An empty provider key is rejected just like an unknown one.
func TestDefaultRemoteConfigBuilder_ConfigMapRef_EmptyProvider(t *testing.T) {
	t.Parallel()
	const ns = "team-a"
	r := builderWith(t, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "cloud"},
		Data:       map[string]string{"cluster": "some-cluster"},
	})
	b := defaultRemoteConfigBuilder{r: r}
	_, _, err := b.RESTConfig(context.Background(), &fluxmeta.KubeConfigReference{
		ConfigMapRef: &fluxmeta.LocalObjectReference{Name: "cloud"},
	}, ns)
	if !errors.Is(err, errInvalidKubeConfigSpec) {
		t.Fatalf("err = %v, want errInvalidKubeConfigSpec", err)
	}
}

// A configMapRef pointing at a ConfigMap that does not exist is terminal.
func TestDefaultRemoteConfigBuilder_ConfigMapRef_Missing(t *testing.T) {
	t.Parallel()
	const ns = "team-a"
	r := builderWith(t)
	b := defaultRemoteConfigBuilder{r: r}
	_, _, err := b.RESTConfig(context.Background(), &fluxmeta.KubeConfigReference{
		ConfigMapRef: &fluxmeta.LocalObjectReference{Name: "absent"},
	}, ns)
	if !errors.Is(err, errInvalidKubeConfigSpec) {
		t.Fatalf("err = %v, want errInvalidKubeConfigSpec", err)
	}
}

// A kubeConfig with neither ref set is a terminal spec error (the seam's own
// guard; admission and ValidateSpec catch this earlier in the live path).
func TestDefaultRemoteConfigBuilder_NoRef(t *testing.T) {
	t.Parallel()
	r := builderWith(t)
	b := defaultRemoteConfigBuilder{r: r}
	_, _, err := b.RESTConfig(context.Background(), &fluxmeta.KubeConfigReference{}, "team-a")
	if !errors.Is(err, errInvalidKubeConfigSpec) {
		t.Fatalf("err = %v, want errInvalidKubeConfigSpec", err)
	}
}

// The configMapRef cache key embeds the ConfigMap's resourceVersion so an
// in-place edit (a changed provider/cluster/region) rebuilds the target-cluster
// connection instead of being masked until token rotation or a restart. The key
// is computed before the cloud auth dispatch, so this exercises it via the
// resourceVersion helper that feeds the key — no cloud account needed.
func TestConfigMapResourceVersion_TracksEdits(t *testing.T) {
	t.Parallel()
	const ns = "team-a"
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "cloud"},
		Data:       map[string]string{"cluster": "one"},
	}
	r := builderWith(t, cm)

	v1, err := r.configMapResourceVersion(context.Background(), ns, "cloud")
	if err != nil {
		t.Fatalf("configMapResourceVersion err = %v", err)
	}
	if v1 == "" {
		t.Fatal("resourceVersion is empty; the cache key would not track edits")
	}

	// An in-place edit must change the resourceVersion the key folds in.
	var fresh corev1.ConfigMap
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "cloud"}, &fresh); err != nil {
		t.Fatalf("get configmap: %v", err)
	}
	fresh.Data["cluster"] = "two"
	if err := r.Update(context.Background(), &fresh); err != nil {
		t.Fatalf("update configmap: %v", err)
	}
	v2, err := r.configMapResourceVersion(context.Background(), ns, "cloud")
	if err != nil {
		t.Fatalf("configMapResourceVersion (post-edit) err = %v", err)
	}
	if v1 == v2 {
		t.Fatalf("resourceVersion unchanged after edit (%q); an in-place ConfigMap edit would be ignored", v2)
	}
}

// A configMapRef cache key for a missing ConfigMap is terminal — the helper
// surfaces the not-found so RESTConfig wraps it as errInvalidKubeConfigSpec.
func TestConfigMapResourceVersion_Missing(t *testing.T) {
	t.Parallel()
	r := builderWith(t)
	if _, err := r.configMapResourceVersion(context.Background(), "team-a", "absent"); err == nil {
		t.Fatal("configMapResourceVersion err = nil, want a not-found error")
	}
}
