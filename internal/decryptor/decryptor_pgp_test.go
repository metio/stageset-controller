// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package decryptor

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/getsops/sops/v3"
	"github.com/getsops/sops/v3/aes"
	"github.com/getsops/sops/v3/cmd/sops/common"
	"github.com/getsops/sops/v3/cmd/sops/formats"
	"github.com/getsops/sops/v3/config"
	"github.com/getsops/sops/v3/keyservice"
	"github.com/getsops/sops/v3/pgp"
)

// newPGPEntity returns a fresh PGP keypair and its armored private key.
func newPGPEntity(t *testing.T) (*openpgp.Entity, string) {
	t.Helper()
	e, err := openpgp.NewEntity("stageset", "pgp decryptor test", "test@example.com", nil)
	if err != nil {
		t.Fatalf("new pgp entity: %v", err)
	}
	var buf bytes.Buffer
	aw, err := armor.Encode(&buf, openpgp.PrivateKeyType, nil)
	if err != nil {
		t.Fatalf("armor private key: %v", err)
	}
	if err := e.SerializePrivate(aw, nil); err != nil {
		t.Fatalf("serialize private key: %v", err)
	}
	if err := aw.Close(); err != nil {
		t.Fatalf("close armor: %v", err)
	}
	return e, buf.String()
}

// pgpEncryptTo encrypts payload to the entity, returning the armored PGP message
// — the same shape SOPS stores a PGP-encrypted data key in.
func pgpEncryptTo(t *testing.T, e *openpgp.Entity, payload []byte) []byte {
	t.Helper()
	var ct bytes.Buffer
	aw, err := armor.Encode(&ct, "PGP MESSAGE", nil)
	if err != nil {
		t.Fatalf("armor: %v", err)
	}
	w, err := openpgp.Encrypt(aw, []*openpgp.Entity{e}, nil, nil, nil)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if _, err := w.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	if err := aw.Close(); err != nil {
		t.Fatalf("close armor: %v", err)
	}
	return ct.Bytes()
}

func decryptRequest(ciphertext []byte) *keyservice.DecryptRequest {
	return &keyservice.DecryptRequest{
		Key:        &keyservice.Key{KeyType: &keyservice.Key_PgpKey{PgpKey: &keyservice.PgpKey{}}},
		Ciphertext: ciphertext,
	}
}

func TestPGPKeyService_DecryptsWithInjectedKey(t *testing.T) {
	entity, asc := newPGPEntity(t)
	dataKey := []byte("a-32-byte-sops-data-key-aaaaaaaa")
	ciphertext := pgpEncryptTo(t, entity, dataKey)

	entities, err := parsePGPKeys([]string{asc})
	if err != nil {
		t.Fatalf("parsePGPKeys: %v", err)
	}
	svc := &pgpKeyService{entities: entities}
	resp, err := svc.Decrypt(context.Background(), decryptRequest(ciphertext))
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(resp.Plaintext, dataKey) {
		t.Fatalf("recovered %q, want %q", resp.Plaintext, dataKey)
	}
}

func TestPGPKeyService_WrongKeyFails(t *testing.T) {
	entity, _ := newPGPEntity(t)
	ciphertext := pgpEncryptTo(t, entity, []byte("secret"))

	_, otherASC := newPGPEntity(t)
	entities, _ := parsePGPKeys([]string{otherASC})
	svc := &pgpKeyService{entities: entities}
	if _, err := svc.Decrypt(context.Background(), decryptRequest(ciphertext)); err == nil {
		t.Fatal("decryption with the wrong PGP key must fail")
	}
}

// sopsEncryptPGP encrypts plaintext YAML to a SOPS file for the PGP fingerprint,
// reading the public key from the GnuPG pubring under GNUPGHOME.
func sopsEncryptPGP(t *testing.T, fingerprint, plaintext string) string {
	t.Helper()
	store := common.StoreForFormat(formats.Yaml, config.NewStoresConfig())
	branches, err := store.LoadPlainFile([]byte(plaintext))
	if err != nil {
		t.Fatalf("load plain: %v", err)
	}
	mk := pgp.NewMasterKeyFromFingerprint(fingerprint)
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

// A real SOPS-PGP file decrypts through DecryptFiles with only the injected
// armored key — no gpg binary, no keyring on disk for the decrypt side.
func TestDecryptor_PGPEndToEnd(t *testing.T) {
	entity, asc := newPGPEntity(t)

	// sops encrypts to a fingerprint by reading the public key from a pubring
	// under GNUPGHOME; write one for the encrypt side only.
	home := t.TempDir()
	pub, err := os.Create(filepath.Join(home, "pubring.gpg"))
	if err != nil {
		t.Fatalf("create pubring: %v", err)
	}
	if err := entity.Serialize(pub); err != nil {
		t.Fatalf("serialize public key: %v", err)
	}
	if err := pub.Close(); err != nil {
		t.Fatalf("close pubring: %v", err)
	}
	t.Setenv("GNUPGHOME", home)

	fingerprint := fmt.Sprintf("%X", entity.PrimaryKey.Fingerprint)
	encrypted := sopsEncryptPGP(t, fingerprint, plainSecret)

	d, err := New(Keys{PGP: []string{asc}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	out, err := d.DecryptFiles(map[string]string{"secret.yaml": encrypted})
	if err != nil {
		t.Fatalf("DecryptFiles: %v", err)
	}
	if !strings.Contains(out["secret.yaml"], "supersecret") {
		t.Fatalf("decrypted output should restore the plaintext, got:\n%s", out["secret.yaml"])
	}
}

func TestNew_ParsesPGPKeys(t *testing.T) {
	_, asc := newPGPEntity(t)
	if _, err := New(Keys{PGP: []string{asc}}); err != nil {
		t.Fatalf("New with a valid PGP key: %v", err)
	}
	if _, err := New(Keys{PGP: []string{"-----BEGIN PGP PRIVATE KEY BLOCK-----\ngarbage\n-----END PGP PRIVATE KEY BLOCK-----"}}); err == nil {
		t.Fatal("New with a malformed PGP key must error")
	}
}
