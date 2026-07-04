// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package preview

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fluxmeta "github.com/fluxcd/pkg/apis/meta"
	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/decryptor"
	"github.com/metio/stageset-controller/internal/decryptor/decryptortest"
)

const plainCM = `apiVersion: v1
kind: ConfigMap
metadata:
  name: app-config
data:
  password: hunter2-plaintext
`

func decryptionStageSet(ns string) *stagesv1.StageSet {
	return &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "enc"},
		Spec: stagesv1.StageSetSpec{
			Decryption: &stagesv1.Decryption{
				Provider:  "sops",
				SecretRef: &fluxmeta.LocalObjectReference{Name: "sops-keys"},
			},
			Stages: []stagesv1.Stage{{Name: "s1", SourceRef: stagesv1.SourceReference{Name: "ea"}}},
		},
	}
}

// RenderStage must decrypt SOPS sources between fetch and build — the
// controller's order — so the rendered objects carry plaintext.
func TestRenderStage_DecryptsSOPSSources(t *testing.T) {
	t.Parallel()
	identity, recipient := decryptortest.NewAgeKey(t)
	encrypted := decryptortest.EncryptYAML(t, recipient, plainCM)
	if !strings.Contains(encrypted, "ENC[") {
		t.Fatal("fixture must be ciphertext")
	}
	dec, err := decryptor.New(decryptor.Keys{Age: []string{identity}})
	if err != nil {
		t.Fatalf("decryptor: %v", err)
	}

	dir := t.TempDir()
	mustWrite(t, dir+"/cm.yaml", encrypted)
	e := NewEngine(fake.NewClientBuilder().WithScheme(testScheme(t)).Build(), false)
	e.SourceDirs = map[string]string{"": dir}
	e.Decryptor = dec

	ss := decryptionStageSet("ns")
	render, err := e.RenderStage(context.Background(), ss, &ss.Spec.Stages[0])
	if err != nil {
		t.Fatalf("RenderStage: %v", err)
	}
	if len(render.Objects) != 1 {
		t.Fatalf("rendered %d objects, want 1", len(render.Objects))
	}
	data, _, _ := unstructured.NestedString(render.Objects[0].Object, "data", "password")
	if data != "hunter2-plaintext" {
		t.Fatalf("rendered data.password = %q, want the decrypted plaintext", data)
	}
}

// A nil Decryptor must not disturb plain (unencrypted) renders.
func TestRenderStage_NilDecryptorRendersPlainSources(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	mustWrite(t, dir+"/cm.yaml", plainCM)
	e := NewEngine(fake.NewClientBuilder().WithScheme(testScheme(t)).Build(), false)
	e.SourceDirs = map[string]string{"": dir}

	ss := decryptionStageSet("ns")
	render, err := e.RenderStage(context.Background(), ss, &ss.Spec.Stages[0])
	if err != nil {
		t.Fatalf("RenderStage: %v", err)
	}
	if len(render.Objects) != 1 {
		t.Fatalf("rendered %d objects, want 1", len(render.Objects))
	}
}

// A decryptor lacking the right key must fail the render closed — never fall
// through to building the ciphertext.
func TestRenderStage_DecryptFailureFailsClosed(t *testing.T) {
	t.Parallel()
	_, recipient := decryptortest.NewAgeKey(t)
	encrypted := decryptortest.EncryptYAML(t, recipient, plainCM)
	wrongIdentity, _ := decryptortest.NewAgeKey(t)
	dec, err := decryptor.New(decryptor.Keys{Age: []string{wrongIdentity}})
	if err != nil {
		t.Fatalf("decryptor: %v", err)
	}

	dir := t.TempDir()
	mustWrite(t, dir+"/cm.yaml", encrypted)
	e := NewEngine(fake.NewClientBuilder().WithScheme(testScheme(t)).Build(), false)
	e.SourceDirs = map[string]string{"": dir}
	e.Decryptor = dec

	ss := decryptionStageSet("ns")
	_, err = e.RenderStage(context.Background(), ss, &ss.Spec.Stages[0])
	if err == nil {
		t.Fatal("a failed decrypt must fail the render, not emit ciphertext")
	}
	if !strings.Contains(err.Error(), `decrypt stage "s1"`) {
		t.Fatalf("error should name the decrypt step and stage, got: %v", err)
	}
}

// BuildDecryptor mirrors the controller's construction from spec.decryption.
func TestBuildDecryptor(t *testing.T) {
	t.Parallel()

	t.Run("no decryption yields nil", func(t *testing.T) {
		t.Parallel()
		ss := decryptionStageSet("ns")
		ss.Spec.Decryption = nil
		dec, err := BuildDecryptor(context.Background(), fake.NewClientBuilder().WithScheme(testScheme(t)).Build(), ss)
		if err != nil || dec != nil {
			t.Fatalf("got (%v, %v), want (nil, nil)", dec, err)
		}
	})

	t.Run("unsupported provider is rejected", func(t *testing.T) {
		t.Parallel()
		ss := decryptionStageSet("ns")
		ss.Spec.Decryption.Provider = "vault"
		_, err := BuildDecryptor(context.Background(), fake.NewClientBuilder().WithScheme(testScheme(t)).Build(), ss)
		if err == nil || !strings.Contains(err.Error(), "vault") {
			t.Fatalf("want an unsupported-provider error naming the provider, got %v", err)
		}
	})

	t.Run("missing key secret fails closed", func(t *testing.T) {
		t.Parallel()
		ss := decryptionStageSet("ns")
		_, err := BuildDecryptor(context.Background(), fake.NewClientBuilder().WithScheme(testScheme(t)).Build(), ss)
		if err == nil || !strings.Contains(err.Error(), "sops-keys") {
			t.Fatalf("want a read-key-secret error naming the secret, got %v", err)
		}
	})

	t.Run("keys are read from the secret and decrypt real ciphertext", func(t *testing.T) {
		t.Parallel()
		identity, recipient := decryptortest.NewAgeKey(t)
		sec := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "sops-keys"},
			Data:       map[string][]byte{"identity.agekey": []byte(identity)},
		}
		c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(sec).Build()
		ss := decryptionStageSet("ns")
		dec, err := BuildDecryptor(context.Background(), c, ss)
		if err != nil || dec == nil {
			t.Fatalf("BuildDecryptor: (%v, %v)", dec, err)
		}
		encrypted := decryptortest.EncryptYAML(t, recipient, plainCM)
		out, err := dec.DecryptFiles(map[string]string{"cm.yaml": encrypted})
		if err != nil {
			t.Fatalf("DecryptFiles: %v", err)
		}
		if !strings.Contains(out["cm.yaml"], "hunter2-plaintext") {
			t.Fatal("the built decryptor must decrypt ciphertext for the secret's key")
		}
	})
}
