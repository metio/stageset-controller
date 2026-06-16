// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package decryptor

import (
	"strings"
	"testing"

	fage "filippo.io/age"
	"github.com/getsops/sops/v3"
	"github.com/getsops/sops/v3/aes"
	sopsage "github.com/getsops/sops/v3/age"
	"github.com/getsops/sops/v3/cmd/sops/common"
	"github.com/getsops/sops/v3/cmd/sops/formats"
	"github.com/getsops/sops/v3/config"
	"github.com/getsops/sops/v3/keyservice"
)

const plainSecret = `apiVersion: v1
kind: Secret
metadata:
  name: db
stringData:
  password: supersecret
`

// newAgeKey returns a fresh (identity, recipient) age keypair for a test.
func newAgeKey(t *testing.T) (identity, recipient string) {
	t.Helper()
	id, err := fage.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate age identity: %v", err)
	}
	return id.String(), id.Recipient().String()
}

// sopsEncryptYAML encrypts plaintext YAML to a SOPS file for the recipient.
func sopsEncryptYAML(t *testing.T, recipient, plaintext string) string {
	t.Helper()
	store := common.StoreForFormat(formats.Yaml, config.NewStoresConfig())
	branches, err := store.LoadPlainFile([]byte(plaintext))
	if err != nil {
		t.Fatalf("load plain file: %v", err)
	}
	mk, err := sopsage.MasterKeyFromRecipient(recipient)
	if err != nil {
		t.Fatalf("master key from recipient: %v", err)
	}
	tree := sops.Tree{
		Branches: branches,
		Metadata: sops.Metadata{
			Version:   "3.13.1",
			KeyGroups: []sops.KeyGroup{{mk}},
		},
	}
	dataKey, errs := tree.GenerateDataKeyWithKeyServices([]keyservice.KeyServiceClient{keyservice.NewLocalClient()})
	if len(errs) > 0 {
		t.Fatalf("generate data key: %v", errs)
	}
	if err := common.EncryptTree(common.EncryptTreeOpts{Tree: &tree, Cipher: aes.NewCipher(), DataKey: dataKey}); err != nil {
		t.Fatalf("encrypt tree: %v", err)
	}
	out, err := store.EmitEncryptedFile(tree)
	if err != nil {
		t.Fatalf("emit encrypted file: %v", err)
	}
	return string(out)
}

func TestDecryptor_RoundTrip(t *testing.T) {
	identity, recipient := newAgeKey(t)
	encrypted := sopsEncryptYAML(t, recipient, plainSecret)
	if strings.Contains(encrypted, "supersecret") {
		t.Fatal("the encrypted file must not contain the plaintext value")
	}
	if !strings.Contains(encrypted, "ENC[") {
		t.Fatal("the encrypted file should carry SOPS ENC[...] markers")
	}

	d, err := New(Keys{Age: []string{identity}})
	if err != nil {
		t.Fatalf("NewAge: %v", err)
	}
	out, err := d.DecryptFiles(map[string]string{"secret.yaml": encrypted})
	if err != nil {
		t.Fatalf("DecryptFiles: %v", err)
	}
	if !strings.Contains(out["secret.yaml"], "supersecret") {
		t.Fatalf("decrypted output should restore the plaintext, got:\n%s", out["secret.yaml"])
	}
}

func TestDecryptor_PassesThroughNonEncrypted(t *testing.T) {
	identity, _ := newAgeKey(t)
	d, err := New(Keys{Age: []string{identity}})
	if err != nil {
		t.Fatalf("NewAge: %v", err)
	}
	plain := map[string]string{
		"deploy.yaml": "apiVersion: apps/v1\nkind: Deployment\n",
		"notes.txt":   "just text, not a structured format",
	}
	out, err := d.DecryptFiles(plain)
	if err != nil {
		t.Fatalf("DecryptFiles: %v", err)
	}
	for name, want := range plain {
		if out[name] != want {
			t.Errorf("%s changed: got %q, want %q", name, out[name], want)
		}
	}
}

func TestDecryptor_MixedSet(t *testing.T) {
	identity, recipient := newAgeKey(t)
	encrypted := sopsEncryptYAML(t, recipient, plainSecret)
	d, _ := New(Keys{Age: []string{identity}})
	out, err := d.DecryptFiles(map[string]string{
		"secret.yaml": encrypted,
		"plain.yaml":  "kind: ConfigMap\n",
	})
	if err != nil {
		t.Fatalf("DecryptFiles: %v", err)
	}
	if !strings.Contains(out["secret.yaml"], "supersecret") {
		t.Error("encrypted entry not decrypted")
	}
	if out["plain.yaml"] != "kind: ConfigMap\n" {
		t.Error("plain entry must pass through untouched")
	}
}

func TestDecryptor_WrongKeyFails(t *testing.T) {
	_, recipient := newAgeKey(t)
	encrypted := sopsEncryptYAML(t, recipient, plainSecret)
	otherIdentity, _ := newAgeKey(t)
	d, _ := New(Keys{Age: []string{otherIdentity}})
	if _, err := d.DecryptFiles(map[string]string{"secret.yaml": encrypted}); err == nil {
		t.Fatal("decryption with the wrong identity must fail")
	}
}

// New with no age identities is valid — a KMS-only setup leans on the stock
// local key service. Such a decryptor still passes non-encrypted files through;
// an age file simply fails to decrypt (no key), which is the wrong-key case.
func TestNew_NoIdentitiesIsKMSOnly(t *testing.T) {
	d, err := New(Keys{})
	if err != nil {
		t.Fatalf("New(Keys{}) should succeed for a KMS-only decryptor: %v", err)
	}
	out, err := d.DecryptFiles(map[string]string{"plain.yaml": "kind: ConfigMap\n"})
	if err != nil {
		t.Fatalf("DecryptFiles: %v", err)
	}
	if out["plain.yaml"] != "kind: ConfigMap\n" {
		t.Error("non-encrypted file must pass through")
	}

	_, recipient := newAgeKey(t)
	encrypted := sopsEncryptYAML(t, recipient, plainSecret)
	if _, err := d.DecryptFiles(map[string]string{"secret.yaml": encrypted}); err == nil {
		t.Fatal("an age file must fail to decrypt when no age identity is configured")
	}
}

func TestNew_ParsesIdentities(t *testing.T) {
	identity, _ := newAgeKey(t)
	if _, err := New(Keys{Age: []string{identity}}); err != nil {
		t.Fatalf("New with a valid identity: %v", err)
	}
	if _, err := New(Keys{Age: []string{"not-an-age-key"}}); err == nil {
		t.Fatal("New with a malformed identity must error")
	}
}
