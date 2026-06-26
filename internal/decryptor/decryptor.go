// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

// Package decryptor decrypts SOPS-encrypted files in a fetched artifact before
// the manifests are built and applied. Decryption happens at build time so an
// encrypted Secret renders to its plaintext form for server-side apply.
//
// Plaintext handling — where decrypted Secret data lives, so operators can size
// the trust boundary correctly:
//
//   - During the kustomize build the decrypted files are written to a private
//     temp tree (mode-0700 dir, 0600 files) and removed when the build returns;
//     they are not held purely in memory.
//   - The rollback store keeps rendered output, which can include that plaintext.
//     The S3 backend encrypts it at rest via server-side encryption
//     (--rollback-store-s3-sse, on by default). The filesystem backend writes it
//     verbatim and adds no application-level encryption — at-rest protection
//     there rests on the PersistentVolume's own encryption and RBAC, so use an
//     encrypted volume (or the S3 backend) when the store holds Secret data.
//
// Three key paths resolve a file's data key:
//
//   - in-cluster age identities (ageKeyService) and PGP private keys
//     (pgpKeyService), both supplied as raw key strings the caller reads from a
//     tenant Secret — so a tenant can only decrypt with material its
//     ServiceAccount can read. PGP is pure Go (ProtonMail/go-crypto): no gpg
//     binary and no GnuPG keyring; and
//   - cloud KMS (AWS/GCP/Azure). By default the stock SOPS local key service
//     resolves these through the controller's ambient credentials (e.g. IRSA),
//     so cloud KMS uses the controller's identity, not the tenant's — matching
//     Flux's kustomize-controller. When a CredentialSource is supplied (the
//     opt-in object-level-KMS path), a per-tenant key service instead injects
//     the tenant ServiceAccount's federated cloud identity into each KMS master
//     key, so KMS decryption is bounded by the tenant's cloud IAM grants rather
//     than the controller's.
package decryptor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/getsops/sops/v3"
	"github.com/getsops/sops/v3/aes"
	"github.com/getsops/sops/v3/age"
	"github.com/getsops/sops/v3/azkv"
	"github.com/getsops/sops/v3/cmd/sops/common"
	"github.com/getsops/sops/v3/cmd/sops/formats"
	"github.com/getsops/sops/v3/config"
	"github.com/getsops/sops/v3/gcpkms"
	"github.com/getsops/sops/v3/keyservice"
	"github.com/getsops/sops/v3/kms"
	"golang.org/x/oauth2"
	"google.golang.org/grpc"
)

// CredentialSource resolves per-tenant cloud credentials for KMS decryption.
// Each method returns the SOPS-compatible credential adapter for one cloud,
// already scoped to the tenant identity the source was constructed for (in
// production, the StageSet's decryption ServiceAccount federated to a cloud
// identity). It is the testability seam: production wires a source backed by
// fluxcd/pkg/auth; tests inject a fake that returns canned adapters and records
// the calls, so the resolution + wiring is exercised without any cloud account.
//
// The adapters returned here only reach the cloud when a master key's Decrypt()
// runs — that step is cloud-CI-only. Selecting the source and handing its
// adapters to the master keys is the account-free, unit-tested part.
type CredentialSource interface {
	// AWS returns the AWS credentials provider for KMS master keys.
	AWS(ctx context.Context) awssdk.CredentialsProvider
	// GCP returns the OAuth2 token source for GCP KMS master keys.
	GCP(ctx context.Context) oauth2.TokenSource
	// Azure returns the token credential for Azure Key Vault master keys.
	Azure(ctx context.Context) azcore.TokenCredential
}

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
// injected age identities first, then either the stock local service (ambient
// KMS) or a per-tenant KMS service (object-level KMS) for cloud master keys.
type Decryptor struct {
	keyServices []keyservice.KeyServiceClient
}

// Option configures New. The zero set keeps the default ambient-KMS behavior.
type Option func(*options)

// options holds the resolved settings for New.
type options struct {
	creds CredentialSource
}

// WithCredentialSource opts into object-level KMS: cloud KMS master keys are
// decrypted with the per-tenant credentials src returns, instead of the
// controller's ambient credentials. A nil src is ignored (ambient behavior is
// kept), so callers can thread the option unconditionally.
func WithCredentialSource(src CredentialSource) Option {
	return func(o *options) { o.creds = src }
}

// New builds a Decryptor from tenant key material. age and PGP keys are resolved
// in-process against the supplied identities (so they stay scoped to what the
// tenant's ServiceAccount can read). For cloud KMS, the last key service is
// either the stock local client — resolving KMS through the controller's ambient
// credentials (the default) — or, when WithCredentialSource is supplied, a
// per-tenant kmsKeyService that injects the tenant's federated cloud identity.
// The key set may be empty for a KMS-only setup.
func New(keys Keys, opts ...Option) (*Decryptor, error) {
	var cfg options
	for _, o := range opts {
		o(&cfg)
	}

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

	if cfg.creds != nil {
		// Object-level KMS: the per-tenant KMS service (tenant federated
		// identity) is the ONLY resolver for cloud master keys. We deliberately
		// do NOT append the stock local key service in this mode: it decrypts
		// cloud-KMS keys with the controller's AMBIENT credentials, so SOPS —
		// which tries every key service for a given master key and stops at the
		// first success — would fall through to it for any key the tenant SA is
		// not authorized for, a confused-deputy that silently defeats per-tenant
		// isolation. age and PGP keys are tenant-supplied material handled by the
		// in-process services appended above (not ambient credentials), so the
		// tenant chain is complete; a key type with no tenant-scoped resolver
		// (e.g. Vault via ambient token) correctly fails to decrypt rather than
		// leaking the controller's identity.
		services = append(services, &kmsKeyService{creds: cfg.creds})
	} else {
		// Default path: no per-tenant identity was requested, so the local key
		// service resolves every master-key type with the controller's ambient
		// credentials.
		services = append(services, keyservice.NewLocalClient())
	}
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

// kmsKeyService decrypts cloud KMS data keys with the tenant's federated
// identity instead of the controller's ambient credentials. For each supported
// cloud it reconstructs the SOPS master key from the request and injects the
// per-tenant credential adapter via the key's ApplyToMasterKey before calling
// Decrypt — so the KMS call carries the tenant SA's cloud identity, bounded by
// that identity's IAM grants. Key types it does not handle (PGP, age, Vault,
// HuaweiCloud) return a NotImplemented-style error so DecryptTree falls through
// to the next key service (the local client), preserving the default behavior
// for everything but cloud KMS.
type kmsKeyService struct {
	creds CredentialSource
}

func (s *kmsKeyService) Decrypt(ctx context.Context, req *keyservice.DecryptRequest, _ ...grpc.CallOption) (*keyservice.DecryptResponse, error) {
	switch k := req.Key.KeyType.(type) {
	case *keyservice.Key_KmsKey:
		mk := kms.NewMasterKeyFromArn(k.KmsKey.Arn, kmsContext(k.KmsKey.Context), k.KmsKey.Role)
		kms.NewCredentialsProvider(s.creds.AWS(ctx)).ApplyToMasterKey(mk)
		mk.EncryptedKey = string(req.Ciphertext)
		plaintext, err := mk.DecryptContext(ctx)
		if err != nil {
			return nil, err
		}
		return &keyservice.DecryptResponse{Plaintext: plaintext}, nil
	case *keyservice.Key_GcpKmsKey:
		mk := gcpkms.MasterKey{ResourceID: k.GcpKmsKey.ResourceId}
		gcpkms.NewTokenSource(s.creds.GCP(ctx)).ApplyToMasterKey(&mk)
		mk.EncryptedKey = string(req.Ciphertext)
		plaintext, err := mk.DecryptContext(ctx)
		if err != nil {
			return nil, err
		}
		return &keyservice.DecryptResponse{Plaintext: plaintext}, nil
	case *keyservice.Key_AzureKeyvaultKey:
		mk := azkv.MasterKey{
			VaultURL: k.AzureKeyvaultKey.VaultUrl,
			Name:     k.AzureKeyvaultKey.Name,
			Version:  k.AzureKeyvaultKey.Version,
		}
		azkv.NewTokenCredential(s.creds.Azure(ctx)).ApplyToMasterKey(&mk)
		mk.EncryptedKey = string(req.Ciphertext)
		plaintext, err := mk.DecryptContext(ctx)
		if err != nil {
			return nil, err
		}
		return &keyservice.DecryptResponse{Plaintext: plaintext}, nil
	default:
		// Not a cloud KMS key this service owns; let the next key service try.
		return nil, fmt.Errorf("decryptor: kms key service does not handle %T", req.Key.KeyType)
	}
}

func (s *kmsKeyService) Encrypt(_ context.Context, _ *keyservice.EncryptRequest, _ ...grpc.CallOption) (*keyservice.EncryptResponse, error) {
	return nil, errors.New("decryptor: encryption is not supported")
}

// kmsContext converts the keyservice encryption-context map into the
// map[string]*string shape SOPS' AWS master key expects.
func kmsContext(in map[string]string) map[string]*string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]*string, len(in))
	for k, v := range in {
		out[k] = &v
	}
	return out
}
