// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fluxmeta "github.com/fluxcd/pkg/apis/meta"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

// TestWebhook_RejectsInvalidAction stands up a dedicated envtest apiserver with
// the generated ValidatingWebhookConfiguration installed and a manager serving
// the webhook, then proves the apiserver rejects an invalid action at admission
// time (not merely that the reconciler later notices). It uses its own
// environment so the webhook plumbing does not affect the shared client tests.
func TestWebhook_RejectsInvalidAction(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("envtest assets unavailable (set KUBEBUILDER_ASSETS or run inside the dev shell)")
	}
	crdDir, err := repoCRDDir()
	if err != nil {
		t.Fatalf("locate config/crd: %v", err)
	}
	webhookDir := filepath.Join(filepath.Dir(crdDir), "webhook")

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{crdDir},
		ErrorIfCRDPathMissing: true,
		CRDs:                  []*apiextv1.CustomResourceDefinition{externalArtifactStubCRD()},
		WebhookInstallOptions: envtest.WebhookInstallOptions{Paths: []string{webhookDir}},
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("envtest start: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	scheme := testScheme(t)
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		WebhookServer: webhook.NewServer(webhook.Options{
			Host:    env.WebhookInstallOptions.LocalServingHost,
			Port:    env.WebhookInstallOptions.LocalServingPort,
			CertDir: env.WebhookInstallOptions.LocalServingCertDir,
		}),
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	if err := (&StageSetValidator{}).SetupWebhookWithManager(mgr); err != nil {
		t.Fatalf("webhook setup: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = mgr.Start(ctx) }()

	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	ns := newNamespace(t, c)

	valid := func(name string) *stagesv1.StageSet {
		return &stagesv1.StageSet{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
			Spec: stagesv1.StageSetSpec{
				Interval: metav1.Duration{Duration: time.Minute},
				Stages: []stagesv1.Stage{{
					Name:      "s",
					SourceRef: stagesv1.SourceReference{Name: "x"},
					// A schema-valid action with exactly one type set.
					Actions: &stagesv1.StageActions{Pre: []stagesv1.Action{{Name: "ok", Wait: &stagesv1.WaitAction{}}}},
				}},
			},
		}
	}

	// Wait for the webhook to serve by retrying a valid create (failurePolicy
	// is Fail, so an unreachable webhook would also reject — we must confirm it
	// is up and accepting first).
	var lastErr error
	ready := false
	for i := range 100 {
		if err := c.Create(ctx, valid(fmt.Sprintf("valid-%d", i))); err == nil {
			ready = true
			break
		} else {
			lastErr = err
			time.Sleep(100 * time.Millisecond)
		}
	}
	if !ready {
		t.Fatalf("webhook never accepted a valid StageSet: %v", lastErr)
	}

	// An action with no type set must be rejected by the webhook itself.
	bad := valid("invalid")
	bad.Spec.Stages[0].Actions.Pre = []stagesv1.Action{{Name: "noop"}}
	err = c.Create(ctx, bad)
	if err == nil {
		t.Fatal("webhook accepted a StageSet whose action sets no type")
	}
	if !strings.Contains(err.Error(), "exactly one of") {
		t.Fatalf("rejection should cite the action oneof rule, got: %v", err)
	}

	// kubeConfig.configMapRef (cloud-provider auth) is accepted at admission —
	// the webhook only shape-checks; the ConfigMap's provider/keys are validated
	// at reconcile time. A named secretRef (self-contained kubeconfig) is
	// accepted too.
	cmRef := valid("kubeconfig-cmref")
	cmRef.Spec.KubeConfig = &fluxmeta.KubeConfigReference{ConfigMapRef: &fluxmeta.LocalObjectReference{Name: "cloud-auth"}}
	if err := c.Create(ctx, cmRef); err != nil {
		t.Fatalf("webhook rejected a StageSet with kubeConfig.configMapRef: %v", err)
	}

	// A configMapRef without a name is rejected at admission (shape check).
	cmRefNoName := valid("kubeconfig-cmref-noname")
	cmRefNoName.Spec.KubeConfig = &fluxmeta.KubeConfigReference{ConfigMapRef: &fluxmeta.LocalObjectReference{}}
	if err := c.Create(ctx, cmRefNoName); err == nil {
		t.Fatal("webhook accepted a StageSet with a nameless kubeConfig.configMapRef")
	} else if !strings.Contains(err.Error(), "configMapRef.name") {
		t.Fatalf("rejection should cite configMapRef.name, got: %v", err)
	}

	secRef := valid("kubeconfig-secretref")
	secRef.Spec.KubeConfig = &fluxmeta.KubeConfigReference{SecretRef: &fluxmeta.SecretKeyReference{Name: "remote-kubeconfig"}}
	if err := c.Create(ctx, secRef); err != nil {
		t.Fatalf("webhook rejected a StageSet with kubeConfig.secretRef: %v", err)
	}
}
