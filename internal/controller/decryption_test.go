// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	fage "filippo.io/age"
	"github.com/fluxcd/pkg/apis/meta"
	"github.com/getsops/sops/v3"
	"github.com/getsops/sops/v3/aes"
	sopsage "github.com/getsops/sops/v3/age"
	"github.com/getsops/sops/v3/cmd/sops/common"
	"github.com/getsops/sops/v3/cmd/sops/formats"
	"github.com/getsops/sops/v3/config"
	"github.com/getsops/sops/v3/keyservice"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

// sopsEncrypt produces a SOPS-encrypted YAML for the recipient.
func sopsEncrypt(t *testing.T, recipient, plaintext string) string {
	t.Helper()
	store := common.StoreForFormat(formats.Yaml, config.NewStoresConfig())
	branches, err := store.LoadPlainFile([]byte(plaintext))
	if err != nil {
		t.Fatalf("load plain: %v", err)
	}
	mk, err := sopsage.MasterKeyFromRecipient(recipient)
	if err != nil {
		t.Fatalf("master key: %v", err)
	}
	tree := sops.Tree{Branches: branches, Metadata: sops.Metadata{Version: "3.13.1", KeyGroups: []sops.KeyGroup{{mk}}}}
	dataKey, errs := tree.GenerateDataKeyWithKeyServices([]keyservice.KeyServiceClient{keyservice.NewLocalClient()})
	if len(errs) > 0 {
		t.Fatalf("data key: %v", errs)
	}
	if err := common.EncryptTree(common.EncryptTreeOpts{Tree: &tree, Cipher: aes.NewCipher(), DataKey: dataKey}); err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	out, err := store.EmitEncryptedFile(tree)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	return string(out)
}

// A SOPS-encrypted Secret in a stage's source is decrypted before apply, so the
// applied Secret carries the plaintext.
func TestReconcile_Decryption_AppliesDecryptedSecret(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)

	id, err := fage.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("age keygen: %v", err)
	}
	secretManifest := "apiVersion: v1\nkind: Secret\nmetadata:\n  name: db\n  namespace: " + ns +
		"\nstringData:\n  password: supersecret\n"
	encrypted := sopsEncrypt(t, id.Recipient().String(), secretManifest)
	servedArtifact(t, c, ns, "ea", "", map[string]string{"secret.yaml": encrypted})

	// the age key Secret the controller reads
	if err := c.Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "sops-age"},
		Data:       map[string][]byte{"keys.agekey": []byte(id.String())},
	}); err != nil {
		t.Fatalf("create key secret: %v", err)
	}

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "with-sops"},
		Spec: stagesv1.StageSetSpec{
			Interval:   metav1.Duration{Duration: time.Minute},
			Decryption: &stagesv1.Decryption{Provider: "sops", SecretRef: &meta.LocalObjectReference{Name: "sops-age"}},
			Stages:     []stagesv1.Stage{{Name: "app", SourceRef: stagesv1.SourceReference{Name: "ea"}}},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	reconcileOnce(t, c, ss)

	var applied corev1.Secret
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "db"}, &applied); err != nil {
		t.Fatalf("the decrypted Secret was not applied: %v", err)
	}
	if got := string(applied.Data["password"]); got != "supersecret" {
		t.Fatalf("applied Secret password = %q, want supersecret (decryption did not run)", got)
	}
}

// A missing key Secret fails the stage rather than applying ciphertext.
func TestReconcile_Decryption_MissingKeySecretFails(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	servedArtifact(t, c, ns, "ea", "", map[string]string{"cm.yaml": configMapManifest(ns, "obj")})

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "no-key"},
		Spec: stagesv1.StageSetSpec{
			Interval:   metav1.Duration{Duration: time.Minute},
			Decryption: &stagesv1.Decryption{Provider: "sops", SecretRef: &meta.LocalObjectReference{Name: "absent"}},
			Stages:     []stagesv1.Stage{{Name: "app", SourceRef: stagesv1.SourceReference{Name: "ea"}}},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	// failStage returns the error so controller-runtime backs off (a missing key
	// Secret may be transient); the Ready condition still records the failure.
	if err := reconcileWith(t, c, ss, nil); err == nil {
		t.Fatal("expected a reconcile error for the missing key secret")
	}

	got := getStageSet(t, c, ns, "no-key")
	if r := readyReason(got); r != ReasonStageFailed {
		t.Fatalf("Ready reason = %q, want %q", r, ReasonStageFailed)
	}
	// The tenant-readable status must carry the scrubbed message, not a raw SOPS
	// diagnostic (which can leak MAC values, key fingerprints, recipient IDs).
	if msg := readyMessageOf(got); !strings.Contains(msg, "decryption failed") {
		t.Errorf("Ready message = %q, want it scrubbed to contain %q", msg, "decryption failed")
	}
}
