// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fluxmeta "github.com/fluxcd/pkg/apis/meta"
	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/decryptor/decryptortest"
)

const decryptedMarker = "plaintext-db-password"

// sopsFixture creates the key Secret in the cluster, a spec.decryption
// StageSet, and a --source-dir tree holding a SOPS-encrypted ConfigMap whose
// data carries decryptedMarker once decrypted.
func sopsFixture(t *testing.T, c client.Client, ns, name string) (dir string) {
	t.Helper()
	identity, recipient := decryptortest.NewAgeKey(t)
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "sops-keys"},
		Data:       map[string][]byte{"identity.agekey": []byte(identity)},
	}
	if err := c.Create(context.Background(), sec); err != nil {
		t.Fatalf("create key secret: %v", err)
	}
	plain := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: enc-config\n  namespace: " + ns + "\ndata:\n  password: " + decryptedMarker + "\n"
	encrypted := decryptortest.EncryptYAML(t, recipient, plain)
	if !strings.Contains(encrypted, "ENC[") {
		t.Fatal("fixture must be ciphertext")
	}

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: stagesv1.StageSetSpec{
			Interval: metav1.Duration{Duration: 5 * time.Minute},
			Decryption: &stagesv1.Decryption{
				Provider:  "sops",
				SecretRef: &fluxmeta.LocalObjectReference{Name: "sops-keys"},
			},
			Stages: []stagesv1.Stage{{Name: "first", SourceRef: stagesv1.SourceReference{Name: "ea"}}},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	return writeSourceTree(t, map[string]string{"cm.yaml": encrypted})
}

// build must decrypt spec.decryption sources: the rendered manifest carries the
// plaintext, never the SOPS markers the controller would not apply.
func TestBuild_DecryptsSOPSSources(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "sopsbuild")
	dir := sopsFixture(t, c, ns, "enc")

	stdout, stderr, code := runCLI(t, cfg, "build", "enc", "-n", ns, "--source-dir", dir)
	if code != exitOK {
		t.Fatalf("build exit = %d (stderr=%s)", code, stderr)
	}
	if !strings.Contains(stdout, decryptedMarker) {
		t.Fatalf("build output must carry the decrypted value:\n%s", stdout)
	}
	if strings.Contains(stdout, "ENC[") || strings.Contains(stdout, "sops:") {
		t.Fatalf("build output leaked ciphertext/SOPS metadata:\n%s", stdout)
	}
}

// diff must decrypt too, or every spec.decryption StageSet permanently shows
// drift on content the controller considers synced.
func TestDiff_DecryptsSOPSSources(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "sopsdiff")
	dir := sopsFixture(t, c, ns, "enc")

	stdout, stderr, code := runCLI(t, cfg, "diff", "enc", "-n", ns, "--source-dir", dir, "--color", "never")
	if code != exitDiff { // the object does not exist yet: changes present
		t.Fatalf("diff exit = %d, want %d (stderr=%s)", code, exitDiff, stderr)
	}
	if !strings.Contains(stdout, decryptedMarker) {
		t.Fatalf("diff output must show the decrypted value the controller would apply:\n%s", stdout)
	}
	if strings.Contains(stdout, "ENC[") {
		t.Fatalf("diff output shows ciphertext:\n%s", stdout)
	}
}

// apply must SSA the decrypted content — applying ciphertext would hand the
// controller permanent drift and publish SOPS metadata into the cluster.
func TestApply_DecryptsSOPSSources(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "sopsapply")
	dir := sopsFixture(t, c, ns, "enc")

	_, stderr, code := runCLI(t, cfg, "apply", "enc", "-n", ns, "--source-dir", dir, "--wait=false")
	if code != exitOK {
		t.Fatalf("apply exit = %d (stderr=%s)", code, stderr)
	}
	var cm corev1.ConfigMap
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "enc-config"}, &cm); err != nil {
		t.Fatalf("get applied ConfigMap: %v", err)
	}
	if cm.Data["password"] != decryptedMarker {
		t.Fatalf("applied data.password = %q, want the decrypted plaintext", cm.Data["password"])
	}
}

// A spec.decryption StageSet whose key secret is missing must fail closed with
// a clear error — silently rendering ciphertext is the bug this pins.
func TestBuild_MissingKeySecretFailsClosed(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "sopsmissing")
	dir := sopsFixture(t, c, ns, "enc")
	// Remove the key secret out from under the StageSet.
	if err := c.Delete(context.Background(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "sops-keys"}}); err != nil {
		t.Fatalf("delete key secret: %v", err)
	}

	stdout, stderr, code := runCLI(t, cfg, "build", "enc", "-n", ns, "--source-dir", dir)
	if code != exitError {
		t.Fatalf("build exit = %d, want %d (runtime failure)", code, exitError)
	}
	if !strings.Contains(stderr, "sops-keys") {
		t.Fatalf("error should name the missing key secret, got: %s", stderr)
	}
	if strings.Contains(stdout, "ENC[") {
		t.Fatalf("no ciphertext may be emitted on a failed decrypt:\n%s", stdout)
	}
}

// A key that cannot decrypt the source fails the render, not falls through.
func TestBuild_WrongKeyFailsClosed(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "sopswrongkey")
	dir := sopsFixture(t, c, ns, "enc")
	// Replace the key material with a fresh identity that never saw the file.
	wrongIdentity, _ := decryptortest.NewAgeKey(t)
	var sec corev1.Secret
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "sops-keys"}, &sec); err != nil {
		t.Fatalf("get key secret: %v", err)
	}
	sec.Data = map[string][]byte{"identity.agekey": []byte(wrongIdentity)}
	if err := c.Update(context.Background(), &sec); err != nil {
		t.Fatalf("swap key secret: %v", err)
	}

	stdout, _, code := runCLI(t, cfg, "build", "enc", "-n", ns, "--source-dir", dir)
	if code != exitError {
		t.Fatalf("build exit = %d, want %d (decrypt failure is a runtime failure)", code, exitError)
	}
	if strings.Contains(stdout, "ENC[") {
		t.Fatalf("no ciphertext may be emitted on a failed decrypt:\n%s", stdout)
	}
}

// A StageSet without spec.decryption renders exactly as before — the wiring
// must not disturb the plain path.
func TestBuild_NoDecryptionUnchanged(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "sopsnone")
	makeStageSet(t, c, ns, "plain")
	dir := writeSourceTree(t, map[string]string{
		"cm.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: settings\ndata:\n  k: v\n",
	})

	stdout, stderr, code := runCLI(t, cfg, "build", "plain", "-n", ns, "--source-dir", dir)
	if code != exitOK {
		t.Fatalf("build exit = %d (stderr=%s)", code, stderr)
	}
	if !strings.Contains(stdout, "name: settings") {
		t.Fatalf("plain build output unexpected:\n%s", stdout)
	}
}
