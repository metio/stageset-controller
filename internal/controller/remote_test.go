// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
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
	"sigs.k8s.io/controller-runtime/pkg/client"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
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

// A cloud-provider configMapRef kubeConfig is explicitly not supported yet and
// must be rejected rather than silently ignored.
func TestReconcile_KubeConfig_ConfigMapRefRejected(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	servedArtifact(t, c, ns, "cm-art", "", map[string]string{"cm.yaml": configMapManifest(ns, "never")})

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "cmref"},
		Spec: stagesv1.StageSetSpec{
			Interval:   metav1.Duration{Duration: 5 * time.Minute},
			KubeConfig: &fluxmeta.KubeConfigReference{ConfigMapRef: &fluxmeta.LocalObjectReference{Name: "cloud"}},
			Stages:     []stagesv1.Stage{{Name: "stage-a", SourceRef: stagesv1.SourceReference{Name: "cm-art"}}},
		},
	}
	mustCreate(t, c, ss)
	reconcileWithConfig(t, c, ss)

	if r := readyReason(getStageSet(t, c, ns, "cmref")); r != ReasonStageFailed {
		t.Fatalf("Ready reason = %q, want %q", r, ReasonStageFailed)
	}
}
