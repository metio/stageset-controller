// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"net/http"
	"testing"
	"time"

	fluxmeta "github.com/fluxcd/pkg/apis/meta"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apitypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/artifact"
)

// kubeconfigFor serializes a rest.Config into a self-contained kubeconfig. The
// envtest server is the "remote" cluster: pointing spec.kubeConfig back at it
// exercises the whole secretRef path (read Secret, parse kubeconfig, build a
// client + dynamic RESTMapper, apply through it) against a real apiserver.
func kubeconfigFor(t *testing.T, cfg *rest.Config) []byte {
	t.Helper()
	api := clientcmdapi.NewConfig()
	api.Clusters["env"] = &clientcmdapi.Cluster{
		Server:                   cfg.Host,
		CertificateAuthorityData: cfg.CAData,
		InsecureSkipTLSVerify:    cfg.Insecure,
	}
	api.AuthInfos["env"] = &clientcmdapi.AuthInfo{
		ClientCertificateData: cfg.CertData,
		ClientKeyData:         cfg.KeyData,
		Token:                 cfg.BearerToken,
	}
	api.Contexts["env"] = &clientcmdapi.Context{Cluster: "env", AuthInfo: "env"}
	api.CurrentContext = "env"
	out, err := clientcmd.Write(*api)
	if err != nil {
		t.Fatalf("serialize kubeconfig: %v", err)
	}
	return out
}

func kubeConfigStageSet(t *testing.T, c client.Client, ns, name, secretName, eaName string) *stagesv1.StageSet {
	t.Helper()
	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: stagesv1.StageSetSpec{
			Interval:   metav1.Duration{Duration: 5 * time.Minute},
			KubeConfig: &fluxmeta.KubeConfigReference{SecretRef: &fluxmeta.SecretKeyReference{Name: secretName}},
			Stages:     []stagesv1.Stage{{Name: "stage-a", SourceRef: stagesv1.SourceReference{Name: eaName}}},
		},
	}
	mustCreate(t, c, ss)
	return ss
}

// A StageSet with spec.kubeConfig applies to the cluster the kubeconfig points
// at, while the StageInventory bookkeeping stays on the controller's cluster.
func TestReconcile_KubeConfig_AppliesToTargetCluster(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	mustCreate(t, c, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "remote-kubeconfig"},
		Data:       map[string][]byte{"value": kubeconfigFor(t, envtestConfig(t))},
	})
	servedArtifact(t, c, ns, "cm-art", "", map[string]string{"cm.yaml": configMapManifest(ns, "remote-cm")})

	ss := kubeConfigStageSet(t, c, ns, "remote", "remote-kubeconfig", "cm-art")
	reconcileWithConfig(t, c, ss)

	if r := readyReason(getStageSet(t, c, ns, "remote")); r != ReasonReady {
		t.Fatalf("Ready reason = %q, want %q", r, ReasonReady)
	}
	if !cmExists(t, c, ns, "remote-cm") {
		t.Fatal("ConfigMap should have been applied to the target cluster")
	}
	// Inventory is controller-side, never written to the target via kubeConfig.
	if n := inventoryEntryCount(t, c, ns, "remote", "stage-a"); n != 1 {
		t.Fatalf("StageInventory should be recorded on the controller cluster, got %d entries", n)
	}
}

// kubeConfig and serviceAccountName compose: the apply runs against the
// kubeconfig's cluster AND impersonates the SA, so the SA's RBAC there is the
// ceiling — a Secret the ConfigMap-only SA cannot create is denied.
func TestReconcile_KubeConfig_ComposesWithImpersonation(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	grantConfigMaps(t, c, ns, "deployer")
	mustCreate(t, c, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "remote-kubeconfig"},
		Data:       map[string][]byte{"value": kubeconfigFor(t, envtestConfig(t))},
	})
	servedArtifact(t, c, ns, "secret-art", "", map[string]string{"s.yaml": secretManifest(ns, "blocked")})

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "combo"},
		Spec: stagesv1.StageSetSpec{
			Interval:           metav1.Duration{Duration: 5 * time.Minute},
			ServiceAccountName: "deployer",
			KubeConfig:         &fluxmeta.KubeConfigReference{SecretRef: &fluxmeta.SecretKeyReference{Name: "remote-kubeconfig"}},
			Stages:             []stagesv1.Stage{{Name: "stage-a", SourceRef: stagesv1.SourceReference{Name: "secret-art"}}},
		},
	}
	mustCreate(t, c, ss)
	reconcileWithConfig(t, c, ss)

	if r := readyReason(getStageSet(t, c, ns, "combo")); r != ReasonStageFailed {
		t.Fatalf("Ready reason = %q, want %q (SA impersonation must apply on the remote config)", r, ReasonStageFailed)
	}
	var sec corev1.Secret
	if err := c.Get(context.Background(), apitypes.NamespacedName{Namespace: ns, Name: "blocked"}, &sec); !apierrors.IsNotFound(err) {
		t.Fatalf("a Secret-less SA must not create the object even via kubeConfig, get err = %v", err)
	}
}

// A kubeConfig Secret whose payload is not a valid kubeconfig fails the run at
// connection time, before any stage work.
func TestReconcile_KubeConfig_InvalidKubeconfigFails(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	mustCreate(t, c, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "bad-kubeconfig"},
		Data:       map[string][]byte{"value": []byte("not a kubeconfig")},
	})
	servedArtifact(t, c, ns, "cm-art", "", map[string]string{"cm.yaml": configMapManifest(ns, "never")})

	ss := kubeConfigStageSet(t, c, ns, "bad", "bad-kubeconfig", "cm-art")
	reconcileWithConfig(t, c, ss)

	if r := readyReason(getStageSet(t, c, ns, "bad")); r != ReasonStageFailed {
		t.Fatalf("Ready reason = %q, want %q", r, ReasonStageFailed)
	}
	if cmExists(t, c, ns, "never") {
		t.Fatal("nothing should be applied when the kubeConfig is unusable")
	}
}

// fakeRemoteConfigBuilder is the test seam standing in for the cloud-provider
// (configMapRef) path: instead of minting a token from a cloud STS, it returns
// a rest.Config pointing at the envtest apiserver. This is what lets the whole
// configMapRef apply/prune path be exercised without any cloud account.
type fakeRemoteConfigBuilder struct {
	cfg *rest.Config
	err error
	// gotConfigMap records the configMapRef name the reconciler asked for, so a
	// test can assert the cloud path (not the secretRef path) was taken.
	gotConfigMap string
}

func (b *fakeRemoteConfigBuilder) RESTConfig(_ context.Context, kc *fluxmeta.KubeConfigReference, _ string) (*rest.Config, string, error) {
	if b.err != nil {
		return nil, "", b.err
	}
	if kc.ConfigMapRef != nil {
		b.gotConfigMap = kc.ConfigMapRef.Name
	}
	out := rest.CopyConfig(b.cfg)
	return out, "fake/" + kc.ConfigMapRef.Name, nil
}

// reconcileWithRemoteBuilder runs the reconciler with a fake remoteConfigBuilder
// injected, so a configMapRef kubeConfig routes through the cloud-provider seam
// — pointed at envtest here — exactly as the secretRef path does.
func reconcileWithRemoteBuilder(t *testing.T, c client.Client, ss *stagesv1.StageSet, builder remoteConfigBuilder) {
	t.Helper()
	r := &StageSetReconciler{
		Client:       c,
		Config:       envtestConfig(t),
		RESTMapper:   c.RESTMapper(),
		remoteConfig: builder,
		Fetcher:      &artifact.Fetcher{HTTPClient: http.DefaultClient, URLValidator: artifact.PermissiveHTTPURL, IPValidator: artifact.PermissiveIP},
	}
	wireRealMinter(t, r)
	_, _ = driveReconcile(r, ctrl.Request{
		NamespacedName: apitypes.NamespacedName{Namespace: ss.Namespace, Name: ss.Name},
	})
}

// A configMapRef kubeConfig (cloud-provider auth) applies to the cluster the
// seam resolves, with the rest of the pipeline (apply, inventory bookkeeping)
// behaving identically to the secretRef path. The fake seam stands in for the
// cloud STS so no AWS/Azure/GCP account is needed.
func TestReconcile_KubeConfig_ConfigMapRef_AppliesToTargetCluster(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	servedArtifact(t, c, ns, "cm-art", "", map[string]string{"cm.yaml": configMapManifest(ns, "cloud-cm")})

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "cloud"},
		Spec: stagesv1.StageSetSpec{
			Interval:   metav1.Duration{Duration: 5 * time.Minute},
			KubeConfig: &fluxmeta.KubeConfigReference{ConfigMapRef: &fluxmeta.LocalObjectReference{Name: "cloud-auth"}},
			Stages:     []stagesv1.Stage{{Name: "stage-a", SourceRef: stagesv1.SourceReference{Name: "cm-art"}}},
		},
	}
	mustCreate(t, c, ss)
	builder := &fakeRemoteConfigBuilder{cfg: envtestConfig(t)}
	reconcileWithRemoteBuilder(t, c, ss, builder)

	if builder.gotConfigMap != "cloud-auth" {
		t.Fatalf("seam asked for configMap %q, want %q", builder.gotConfigMap, "cloud-auth")
	}
	if r := readyReason(getStageSet(t, c, ns, "cloud")); r != ReasonReady {
		t.Fatalf("Ready reason = %q, want %q", r, ReasonReady)
	}
	if !cmExists(t, c, ns, "cloud-cm") {
		t.Fatal("ConfigMap should have been applied to the target cluster")
	}
	if n := inventoryEntryCount(t, c, ns, "cloud", "stage-a"); n != 1 {
		t.Fatalf("StageInventory should be recorded on the controller cluster, got %d entries", n)
	}
}

// A configMapRef whose ConfigMap names an unknown provider (or is malformed) is
// terminal: fluxcd/pkg/auth rejects it, the seam wraps it as
// errInvalidKubeConfigSpec, and the reconciler surfaces ReasonInvalidSpec
// without applying anything. This drives the real defaultRemoteConfigBuilder
// against an in-cluster ConfigMap — no cloud account, no network.
func TestReconcile_KubeConfig_ConfigMapRef_InvalidProvider(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	servedArtifact(t, c, ns, "cm-art", "", map[string]string{"cm.yaml": configMapManifest(ns, "never")})
	mustCreate(t, c, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "cloud-auth"},
		Data:       map[string]string{"provider": "nonsense"},
	})

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "badprovider"},
		Spec: stagesv1.StageSetSpec{
			Interval:   metav1.Duration{Duration: 5 * time.Minute},
			KubeConfig: &fluxmeta.KubeConfigReference{ConfigMapRef: &fluxmeta.LocalObjectReference{Name: "cloud-auth"}},
			Stages:     []stagesv1.Stage{{Name: "stage-a", SourceRef: stagesv1.SourceReference{Name: "cm-art"}}},
		},
	}
	mustCreate(t, c, ss)
	// No remoteConfig override: exercise the production defaultRemoteConfigBuilder.
	reconcileWithConfig(t, c, ss)

	if r := readyReason(getStageSet(t, c, ns, "badprovider")); r != ReasonInvalidSpec {
		t.Fatalf("Ready reason = %q, want %q", r, ReasonInvalidSpec)
	}
	if cmExists(t, c, ns, "never") {
		t.Fatal("nothing should be applied when the cloud provider is invalid")
	}
}

// A configMapRef whose ConfigMap does not exist is also terminal: the seam
// can't read it and wraps the failure as errInvalidKubeConfigSpec.
func TestReconcile_KubeConfig_ConfigMapRef_MissingConfigMap(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	servedArtifact(t, c, ns, "cm-art", "", map[string]string{"cm.yaml": configMapManifest(ns, "never")})

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "missingcm"},
		Spec: stagesv1.StageSetSpec{
			Interval:   metav1.Duration{Duration: 5 * time.Minute},
			KubeConfig: &fluxmeta.KubeConfigReference{ConfigMapRef: &fluxmeta.LocalObjectReference{Name: "absent"}},
			Stages:     []stagesv1.Stage{{Name: "stage-a", SourceRef: stagesv1.SourceReference{Name: "cm-art"}}},
		},
	}
	mustCreate(t, c, ss)
	reconcileWithConfig(t, c, ss)

	if r := readyReason(getStageSet(t, c, ns, "missingcm")); r != ReasonInvalidSpec {
		t.Fatalf("Ready reason = %q, want %q", r, ReasonInvalidSpec)
	}
	if cmExists(t, c, ns, "never") {
		t.Fatal("nothing should be applied when the ConfigMap is missing")
	}
}
