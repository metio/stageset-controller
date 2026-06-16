// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"encoding/base64"
	"testing"

	fage "filippo.io/age"
	"github.com/getsops/sops/v3"
	"github.com/getsops/sops/v3/aes"
	sopsage "github.com/getsops/sops/v3/age"
	"github.com/getsops/sops/v3/cmd/sops/common"
	"github.com/getsops/sops/v3/cmd/sops/formats"
	"github.com/getsops/sops/v3/config"
	"github.com/getsops/sops/v3/keyservice"

	"github.com/metio/stageset-controller/internal/build"
	"github.com/metio/stageset-controller/internal/decryptor"
)

// sopsEncryptDotenv encrypts a dotenv body to a SOPS file for the recipient.
func sopsEncryptDotenv(t *testing.T, recipient, plaintext string) string {
	t.Helper()
	store := common.StoreForFormat(formats.Dotenv, config.NewStoresConfig())
	branches, err := store.LoadPlainFile([]byte(plaintext))
	if err != nil {
		t.Fatalf("load plain dotenv: %v", err)
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

// Phase 3: a kustomize secretGenerator fed by a SOPS-encrypted dotenv file works,
// because the file is decrypted before the build, so the generator reads
// plaintext. No special handling beyond the build-time decryption is needed.
func TestDecryption_SecretGeneratorFromEncryptedEnv(t *testing.T) {
	id, err := fage.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("age keygen: %v", err)
	}
	encEnv := sopsEncryptDotenv(t, id.Recipient().String(), "password=supersecret\n")

	files := map[string]string{
		"kustomization.yaml": "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\n" +
			"secretGenerator:\n  - name: app-secret\n    envs:\n      - secrets.env\n" +
			"generatorOptions:\n  disableNameSuffixHash: true\n",
		"secrets.env": encEnv,
	}

	d, err := decryptor.New(decryptor.Keys{Age: []string{id.String()}})
	if err != nil {
		t.Fatalf("decryptor: %v", err)
	}
	dec, err := d.DecryptFiles(files)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	objs, err := build.Build(dec, build.Options{}, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	var found bool
	for _, o := range objs {
		if o.GetKind() != "Secret" || o.GetName() != "app-secret" {
			continue
		}
		found = true
		raw, ok, _ := unstructuredString(o.Object, "data", "password")
		if !ok {
			t.Fatal("generated Secret has no data.password")
		}
		got, derr := base64.StdEncoding.DecodeString(raw)
		if derr != nil {
			t.Fatalf("decode secret data: %v", derr)
		}
		if string(got) != "supersecret" {
			t.Fatalf("generated Secret password = %q, want supersecret", got)
		}
	}
	if !found {
		t.Fatal("the secretGenerator did not produce the expected Secret")
	}
}

func unstructuredString(obj map[string]any, fields ...string) (string, bool, error) {
	cur := any(obj)
	for _, f := range fields {
		m, ok := cur.(map[string]any)
		if !ok {
			return "", false, nil
		}
		cur, ok = m[f]
		if !ok {
			return "", false, nil
		}
	}
	s, ok := cur.(string)
	return s, ok, nil
}
