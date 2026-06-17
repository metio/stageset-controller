// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// envtestMainSetup boots a one-off envtest with the StageSet CRDs installed,
// writes a kubeconfig for it to a tempfile, and points KUBECONFIG at it for the
// duration of the test so ctrl.GetConfig() inside run() resolves to this
// apiserver. Skips cleanly when the envtest asset bundle is unavailable, the
// same selection contract the controller-package envtests use.
func envtestMainSetup(t *testing.T) *envtest.Environment {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("envtest assets unavailable (set KUBEBUILDER_ASSETS or run inside the dev shell)")
	}
	crdDir := mainCRDDir(t)

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{crdDir},
		ErrorIfCRDPathMissing: true,
	}
	if _, err := env.Start(); err != nil {
		t.Fatalf("envtest start: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	user, err := env.AddUser(envtest.User{Name: "admin", Groups: []string{"system:masters"}}, nil)
	if err != nil {
		t.Fatalf("envtest AddUser: %v", err)
	}
	kubeconfig, err := user.KubeConfig()
	if err != nil {
		t.Fatalf("envtest KubeConfig: %v", err)
	}
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, kubeconfig, 0o600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	t.Setenv("KUBECONFIG", path)
	return env
}

// mainCRDDir walks up from the cmd package to the generated CRD manifests under
// config/crd, mirroring repoCRDDir in the controller suite.
func mainCRDDir(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	origin := dir
	for {
		cand := filepath.Join(dir, "config", "crd")
		if fi, statErr := os.Stat(cand); statErr == nil && fi.IsDir() {
			return cand
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("config/crd not found walking up from %s", origin)
		}
		dir = parent
	}
}

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	defer func() { _ = l.Close() }()
	return strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
}

// waitForProbe polls a controller-runtime health probe path until it returns
// 200 or the deadline elapses.
func waitForProbe(t *testing.T, addr, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	url := "http://" + addr + path
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		resp, err := http.DefaultClient.Do(req)
		cancel()
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("probe %s never returned 200 within %s", url, timeout)
}

// runMain launches run() in a goroutine against the envtest apiserver, waits
// for the readiness probe to go green (manager started), then cancels the
// context and asserts a clean exit code 0 within a bound.
func runMain(t *testing.T, extraArgs []string) {
	t.Helper()
	// The apiserver is reached via KUBECONFIG set by envtestMainSetup.
	probeAddr := "127.0.0.1:" + freePort(t)
	args := append([]string{
		"-metrics-bind-address=0",
		"-health-probe-bind-address=" + probeAddr,
		"-gate-bind-address=", // disable the gate server
		"-leader-elect=false",
		"-webhook-port=" + freePort(t),
	}, extraArgs...)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- run(ctx, args, nil, io.Discard) }()

	waitForProbe(t, probeAddr, "/readyz", 60*time.Second)

	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("run exit code = %d, want 0", code)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("run did not return within 60s of context cancellation")
	}
}

// TestRun_SelfSignedWebhook_BootsAndPatchesVWC exercises the chart-default
// self-signed webhook wiring (provisionSelfSignedWebhookCert): run() generates
// an in-pod CA, writes the serving cert to the cert dir, and patches a real
// ValidatingWebhookConfiguration's caBundle through the apiserver before the
// manager starts. The path that inClusterNamespace would resolve is supplied
// explicitly via --webhook-service-namespace, since envtest runs outside a pod.
func TestRun_SelfSignedWebhook_BootsAndPatchesVWC(t *testing.T) {
	env := envtestMainSetup(t)

	clientset, err := kubernetes.NewForConfig(env.Config)
	if err != nil {
		t.Fatalf("clientset: %v", err)
	}
	vwcs := clientset.AdmissionregistrationV1().ValidatingWebhookConfigurations()

	failurePolicy := admissionv1.Fail
	sideEffects := admissionv1.SideEffectClassNone
	const vwcName = "vstageset-test"
	vwc := &admissionv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: vwcName},
		Webhooks: []admissionv1.ValidatingWebhook{{
			Name:                    "vstageset.stages.metio.wtf",
			AdmissionReviewVersions: []string{"v1"},
			FailurePolicy:           &failurePolicy,
			SideEffects:             &sideEffects,
			ClientConfig: admissionv1.WebhookClientConfig{
				Service: &admissionv1.ServiceReference{Name: "stageset-controller-webhook", Namespace: "default"},
				// CABundle deliberately empty — provisionSelfSignedWebhookCert fills it.
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
	if _, err := vwcs.Create(context.Background(), vwc, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create VWC: %v", err)
	}
	t.Cleanup(func() { _ = vwcs.Delete(context.Background(), vwcName, metav1.DeleteOptions{}) })

	certDir := t.TempDir()
	runMain(t, []string{
		"-enable-webhook=true",
		"-webhook-cert-mode=self-signed",
		"-webhook-validating-config-name=" + vwcName,
		"-webhook-service-namespace=default",
		"-webhook-cert-dir=" + certDir,
	})

	// The serving material reached the cert dir.
	for _, f := range []string{"tls.crt", "tls.key"} {
		if _, err := os.Stat(filepath.Join(certDir, f)); err != nil {
			t.Errorf("expected %s in cert dir: %v", f, err)
		}
	}
	// The VWC's caBundle was stamped by the provisioning path.
	got, err := vwcs.Get(context.Background(), vwcName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("re-get VWC: %v", err)
	}
	if len(got.Webhooks) == 0 || len(got.Webhooks[0].ClientConfig.CABundle) == 0 {
		t.Fatalf("VWC caBundle was not stamped: %s", debugVWC(got))
	}
}

func debugVWC(v *admissionv1.ValidatingWebhookConfiguration) string {
	if v == nil {
		return "<nil>"
	}
	return fmt.Sprintf("webhooks=%d", len(v.Webhooks))
}
