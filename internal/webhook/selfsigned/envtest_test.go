// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package selfsigned

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"os"
	"testing"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// placeholderVWC builds a ValidatingWebhookConfiguration with an empty caBundle
// for the patcher to fill.
func placeholderVWC(name, webhookName string) *admissionv1.ValidatingWebhookConfiguration {
	failurePolicy := admissionv1.Fail
	sideEffects := admissionv1.SideEffectClassNone
	return &admissionv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Webhooks: []admissionv1.ValidatingWebhook{{
			Name:                    webhookName,
			AdmissionReviewVersions: []string{"v1"},
			FailurePolicy:           &failurePolicy,
			SideEffects:             &sideEffects,
			ClientConfig: admissionv1.WebhookClientConfig{
				Service: &admissionv1.ServiceReference{Name: "stageset-controller-webhook", Namespace: "default"},
			},
			Rules: []admissionv1.RuleWithOperations{{
				Operations: []admissionv1.OperationType{admissionv1.Create},
				Rule: admissionv1.Rule{
					APIGroups:   []string{"stages.metio.wtf"},
					APIVersions: []string{"v1"},
					Resources:   []string{"stagesets"},
				},
			}},
		}},
	}
}

// TestEnvtest_SelfSignedFlow_PatchesVWCEndToEnd boots a real envtest apiserver,
// applies a placeholder VWC, then drives the full Generate → UpdateVWCCABundle
// pipeline through a real clientset — catching the integration gaps the
// fakeVWCClient hides (optimistic-concurrency Update semantics, real
// resourceVersion handling).
func TestEnvtest_SelfSignedFlow_PatchesVWCEndToEnd(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("envtest assets unavailable (set KUBEBUILDER_ASSETS or run inside the dev shell)")
	}

	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("envtest start: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("clientset: %v", err)
	}
	vwcs := clientset.AdmissionregistrationV1().ValidatingWebhookConfigurations()

	vwc := placeholderVWC("test-selfsigned", "vtest.stages.metio.wtf")
	if _, err := vwcs.Create(context.Background(), vwc, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create VWC: %v", err)
	}
	t.Cleanup(func() { _ = vwcs.Delete(context.Background(), vwc.Name, metav1.DeleteOptions{}) })

	bundle, err := Generate(Input{Namespace: "default"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if err := UpdateVWCCABundle(context.Background(), vwcs, vwc.Name, func(cur []byte) []byte {
		return CombineCABundles(cur, bundle.CABundle)
	}); err != nil {
		t.Fatalf("UpdateVWCCABundle: %v", err)
	}

	got, err := vwcs.Get(context.Background(), vwc.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("re-get VWC: %v", err)
	}
	if len(got.Webhooks) != 1 {
		t.Fatalf("VWC has %d webhooks, want 1", len(got.Webhooks))
	}
	gotCA := got.Webhooks[0].ClientConfig.CABundle
	if string(gotCA) != string(bundle.CABundle) {
		t.Errorf("caBundle mismatch:\n  got:  %s\n  want: %s", gotCA, bundle.CABundle)
	}
	block, _ := pem.Decode(gotCA)
	if block == nil {
		t.Fatal("PEM decode of the stamped caBundle failed")
	}
	if _, err := x509.ParseCertificate(block.Bytes); err != nil {
		t.Errorf("stamped CA does not parse as x509: %v", err)
	}

	// A second identical mutation must short-circuit (no Update issued), so the
	// object's resourceVersion is unchanged.
	before, err := vwcs.Get(context.Background(), vwc.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get before idempotent call: %v", err)
	}
	if err := UpdateVWCCABundle(context.Background(), vwcs, vwc.Name, func(cur []byte) []byte {
		return CombineCABundles(cur, bundle.CABundle)
	}); err != nil {
		t.Fatalf("idempotent UpdateVWCCABundle: %v", err)
	}
	after, err := vwcs.Get(context.Background(), vwc.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get after idempotent call: %v", err)
	}
	if before.ResourceVersion != after.ResourceVersion {
		t.Errorf("idempotent call issued a write: resourceVersion %s → %s", before.ResourceVersion, after.ResourceVersion)
	}
}

// TestEnvtest_SelfSignedFlow_RotateRePatchesVWC walks a full rotation against a
// real apiserver: bootstrap cert → rotate → confirm the caBundle changed and
// still parses. The renewer's per-rotation contract is "produce different
// bytes"; this pins that the patcher writes those bytes through to the apiserver.
func TestEnvtest_SelfSignedFlow_RotateRePatchesVWC(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("envtest assets unavailable (set KUBEBUILDER_ASSETS or run inside the dev shell)")
	}

	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("envtest start: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("clientset: %v", err)
	}
	vwcs := clientset.AdmissionregistrationV1().ValidatingWebhookConfigurations()

	vwc := placeholderVWC("test-rotate", "vrotate.stages.metio.wtf")
	if _, err := vwcs.Create(context.Background(), vwc, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create VWC: %v", err)
	}
	t.Cleanup(func() { _ = vwcs.Delete(context.Background(), vwc.Name, metav1.DeleteOptions{}) })

	r := &Renewer{
		Input:     Input{Namespace: "default"},
		CertDir:   t.TempDir(),
		VWCName:   vwc.Name,
		VWCClient: vwcs,
	}
	if err := r.renewOnce(context.Background()); err != nil {
		t.Fatalf("first renewOnce: %v", err)
	}
	first, err := vwcs.Get(context.Background(), vwc.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get after first rotation: %v", err)
	}
	firstCA := append([]byte(nil), first.Webhooks[0].ClientConfig.CABundle...)
	if len(firstCA) == 0 {
		t.Fatal("first rotation did not populate caBundle")
	}

	if err := r.renewOnce(context.Background()); err != nil {
		t.Fatalf("second renewOnce: %v", err)
	}
	second, err := vwcs.Get(context.Background(), vwc.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get after second rotation: %v", err)
	}
	secondCA := second.Webhooks[0].ClientConfig.CABundle
	if string(firstCA) == string(secondCA) {
		t.Error("second rotation produced an identical caBundle — the renewer is not rolling the CA")
	}
	if block, _ := pem.Decode(secondCA); block == nil {
		t.Fatal("second rotation caBundle is not PEM")
	}
}
