// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

// Package decryptor decrypts SOPS-encrypted files in a fetched artifact before
// the manifests are built and applied. Decryption happens at build time so an
// encrypted Secret renders to its plaintext form for server-side apply; the
// decrypted bytes live only in memory on the apply path. The rollback store,
// which keeps rendered output, is encrypted at rest (see internal/rollbackstore)
// so this never lands plaintext on disk.
//
// Three key paths resolve a file's data key:
//
//   - in-cluster age identities (ageKeyService) and PGP private keys
//     (pgpKeyService), both supplied as raw key strings the caller reads from a
//     tenant Secret — so a tenant can only decrypt with material its
//     ServiceAccount can read. PGP is pure Go (ProtonMail/go-crypto): no gpg
//     binary and no GnuPG keyring; and
//   - the stock SOPS local key service, which resolves cloud KMS
//     (AWS/GCP/Azure/Vault) through the controller's ambient credentials (e.g.
//     IRSA). Cloud KMS therefore uses the controller's identity, not the
//     tenant's — matching Flux's kustomize-controller.
package decryptor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/getsops/sops/v3"
	"github.com/getsops/sops/v3/aes"
	"github.com/getsops/sops/v3/age"
	"github.com/getsops/sops/v3/cmd/sops/common"
	"github.com/getsops/sops/v3/cmd/sops/formats"
	"github.com/getsops/sops/v3/config"
	"github.com/getsops/sops/v3/keyservice"
	"google.golang.org/grpc"
)

// Keys is the decryption key material read from a tenant Secret. Both kinds are
// optional; an empty set yields a KMS-only decryptor.
type Keys struct {
	// Age holds armored age private keys (AGE-SECRET-KEY-…); each entry may
	// hold several newline-separated keys.
	Age []string
	// PGP holds armored PGP private keys (the ".asc" blocks).
	PGP []string
}

// Decryptor decrypts SOPS-encrypted files using an ordered set of key services:
// injected age identities first, then the stock local service for KMS/PGP.
type Decryptor struct {
	keyServices []keyservice.KeyServiceClient
}

// New builds a Decryptor from tenant key material. age and PGP keys are resolved
// in-process against the supplied identities (so they stay scoped to what the
// tenant's ServiceAccount can read); a stock local key service is always appended
// last so cloud KMS resolves through the controller's ambient credentials. The
// key set may be empty for a KMS-only setup.
func New(keys Keys) (*Decryptor, error) {
	var services []keyservice.KeyServiceClient

	var parsedAge age.ParsedIdentities
	for _, id := range keys.Age {
		if strings.TrimSpace(id) == "" {
			continue
		}
		if err := parsedAge.Import(id); err != nil {
			return nil, fmt.Errorf("decryptor: parse age identity: %w", err)
		}
	}
	if len(parsedAge) > 0 {
		services = append(services, &ageKeyService{identities: parsedAge})
	}

	entities, err := parsePGPKeys(keys.PGP)
	if err != nil {
		return nil, err
	}
	if len(entities) > 0 {
		services = append(services, &pgpKeyService{entities: entities})
	}

	services = append(services, keyservice.NewLocalClient())
	return &Decryptor{keyServices: services}, nil
}

// parsePGPKeys parses armored PGP private keys into one entity list.
func parsePGPKeys(keys []string) (openpgp.EntityList, error) {
	var all openpgp.EntityList
	for _, k := range keys {
		if strings.TrimSpace(k) == "" {
			continue
		}
		el, err := openpgp.ReadArmoredKeyRing(strings.NewReader(k))
		if err != nil {
			return nil, fmt.Errorf("decryptor: parse pgp key: %w", err)
		}
		all = append(all, el...)
	}
	return all, nil
}

// DecryptFiles returns a copy of files with every SOPS-encrypted entry decrypted
// in place. Files without SOPS metadata, and non-structured formats, pass through
// unchanged, so the map can be handed straight to the kustomize build.
func (d *Decryptor) DecryptFiles(files map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(files))
	for name, content := range files {
		dec, err := d.decryptFile(name, []byte(content))
		if err != nil {
			return nil, fmt.Errorf("decrypt %q: %w", name, err)
		}
		out[name] = string(dec)
	}
	return out, nil
}

// decryptFile decrypts one file, or returns it unchanged when it carries no SOPS
// metadata or is a format that cannot.
func (d *Decryptor) decryptFile(name string, data []byte) ([]byte, error) {
	format := formats.FormatForPath(name)
	switch format {
	case formats.Yaml, formats.Json, formats.Dotenv, formats.Ini:
		// structured formats that can carry sops metadata
	default:
		return data, nil
	}
	store := common.StoreForFormat(format, config.NewStoresConfig())
	tree, err := store.LoadEncryptedFile(data)
	if err != nil {
		if errors.Is(err, sops.MetadataNotFound) {
			return data, nil // not a SOPS file; leave verbatim
		}
		return nil, err
	}
	if _, err := common.DecryptTree(common.DecryptTreeOpts{
		Tree:        &tree,
		KeyServices: d.keyServices,
		Cipher:      aes.NewCipher(),
	}); err != nil {
		return nil, err
	}
	return store.EmitPlainFile(tree.Branches)
}

// ageKeyService is a minimal SOPS key service that decrypts age data keys with a
// fixed identity set, instead of the stock server's environment lookup — so the
// controller stays concurrency-safe and never reads global SOPS_AGE_KEY state.
type ageKeyService struct {
	identities age.ParsedIdentities
}

func (s *ageKeyService) Decrypt(_ context.Context, req *keyservice.DecryptRequest, _ ...grpc.CallOption) (*keyservice.DecryptResponse, error) {
	k, ok := req.Key.KeyType.(*keyservice.Key_AgeKey)
	if !ok {
		return nil, fmt.Errorf("decryptor: only age keys are supported (got %T)", req.Key.KeyType)
	}
	mk := &age.MasterKey{Recipient: k.AgeKey.Recipient}
	mk.SetEncryptedDataKey(req.Ciphertext)
	s.identities.ApplyToMasterKey(mk)
	plaintext, err := mk.Decrypt()
	if err != nil {
		return nil, err
	}
	return &keyservice.DecryptResponse{Plaintext: plaintext}, nil
}

func (s *ageKeyService) Encrypt(_ context.Context, _ *keyservice.EncryptRequest, _ ...grpc.CallOption) (*keyservice.EncryptResponse, error) {
	return nil, errors.New("decryptor: encryption is not supported")
}

// pgpKeyService decrypts a PGP data key against an in-memory entity list parsed
// from armored private keys — pure Go, so it needs neither the gpg binary nor a
// GnuPG keyring on disk, and the keys stay tenant-scoped (read from the
// StageSet's Secret, never the controller's environment).
type pgpKeyService struct {
	entities openpgp.EntityList
}

func (s *pgpKeyService) Decrypt(_ context.Context, req *keyservice.DecryptRequest, _ ...grpc.CallOption) (*keyservice.DecryptResponse, error) {
	if _, ok := req.Key.KeyType.(*keyservice.Key_PgpKey); !ok {
		return nil, fmt.Errorf("decryptor: pgp key service got %T", req.Key.KeyType)
	}
	block, err := armor.Decode(bytes.NewReader(req.Ciphertext))
	if err != nil {
		return nil, fmt.Errorf("decryptor: armor-decode pgp data key: %w", err)
	}
	md, err := openpgp.ReadMessage(block.Body, s.entities, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("decryptor: read pgp message: %w", err)
	}
	plaintext, err := io.ReadAll(md.UnverifiedBody)
	if err != nil {
		return nil, fmt.Errorf("decryptor: read pgp plaintext: %w", err)
	}
	return &keyservice.DecryptResponse{Plaintext: plaintext}, nil
}

func (s *pgpKeyService) Encrypt(_ context.Context, _ *keyservice.EncryptRequest, _ ...grpc.CallOption) (*keyservice.EncryptResponse, error) {
	return nil, errors.New("decryptor: encryption is not supported")
}
