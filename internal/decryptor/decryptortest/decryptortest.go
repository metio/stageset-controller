// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

// Package decryptortest produces SOPS-encrypted fixtures for tests outside the
// decryptor package — the preview engine and the CLI commands prove their
// decrypt-before-build wiring against real ciphertext, not fakes.
package decryptortest

import (
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

// NewAgeKey returns a fresh (identity, recipient) age keypair.
func NewAgeKey(t *testing.T) (identity, recipient string) {
	t.Helper()
	id, err := fage.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate age identity: %v", err)
	}
	return id.String(), id.Recipient().String()
}

// EncryptYAML encrypts plaintext YAML into a SOPS file for the recipient.
func EncryptYAML(t *testing.T, recipient, plaintext string) string {
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
