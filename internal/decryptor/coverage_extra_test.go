// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package decryptor

import (
	"context"
	"strings"
	"testing"

	"github.com/getsops/sops/v3/keyservice"
)

// New skips empty and whitespace-only age entries, so a key set padded with
// blanks yields a KMS-only decryptor rather than a parse error. The whitespace
// entry exercises the strings.TrimSpace skip in New's age loop.
func TestNew_SkipsBlankAgeEntries(t *testing.T) {
	d, err := New(Keys{Age: []string{"", "   \n\t "}})
	if err != nil {
		t.Fatalf("New with only blank age entries should succeed: %v", err)
	}
	if hasKMSKeyService(d) {
		t.Fatal("blank age entries must not wire the per-tenant kms service")
	}
	// No age service was wired, so a real age file fails to decrypt.
	_, recipient := newAgeKey(t)
	encrypted := sopsEncryptYAML(t, recipient, plainSecret)
	if _, err := d.DecryptFiles(map[string]string{"secret.yaml": encrypted}); err == nil {
		t.Fatal("an age file must fail to decrypt when only blank age entries were supplied")
	}
}

// New skips empty and whitespace-only PGP entries, exercising the TrimSpace skip
// in parsePGPKeys. A blank-only PGP set yields a KMS-only decryptor.
func TestNew_SkipsBlankPGPEntries(t *testing.T) {
	d, err := New(Keys{PGP: []string{"", "  \n  "}})
	if err != nil {
		t.Fatalf("New with only blank PGP entries should succeed: %v", err)
	}
	if hasKMSKeyService(d) {
		t.Fatal("blank PGP entries must not wire the per-tenant kms service")
	}
}

// parsePGPKeys returns an empty list (no error) when every entry is blank, so the
// caller wires no pgp service. Directly exercises the whitespace-skip branch.
func TestParsePGPKeys_AllBlankYieldsNoEntities(t *testing.T) {
	entities, err := parsePGPKeys([]string{"", "\n", "   "})
	if err != nil {
		t.Fatalf("parsePGPKeys with blank entries: %v", err)
	}
	if len(entities) != 0 {
		t.Fatalf("blank PGP entries must yield no entities, got %d", len(entities))
	}
}

// A structured file that is not a SOPS file but is still malformed for its store
// (here, YAML that the encrypted-file loader rejects for reasons other than
// missing SOPS metadata) surfaces the LoadEncryptedFile error branch of
// decryptFile rather than passing through.
func TestDecryptFile_LoadEncryptedFileError(t *testing.T) {
	d, err := New(Keys{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Invalid YAML cannot be parsed into a tree, so LoadEncryptedFile returns an
	// error that is NOT sops.MetadataNotFound — the non-passthrough error path.
	badYAML := "this: : not: valid: yaml: ["
	if _, err := d.DecryptFiles(map[string]string{"broken.yaml": badYAML}); err == nil {
		t.Fatal("malformed YAML must surface a decrypt error, not pass through")
	}
}

// A non-structured format (no SOPS metadata possible) passes through untouched,
// exercising decryptFile's default switch arm for an unsupported extension.
func TestDecryptFile_UnsupportedFormatPassesThrough(t *testing.T) {
	d, err := New(Keys{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	const md = "# a markdown file\n\nnot structured\n"
	out, err := d.DecryptFiles(map[string]string{"README.md": md})
	if err != nil {
		t.Fatalf("DecryptFiles: %v", err)
	}
	if out["README.md"] != md {
		t.Errorf("unsupported format must pass through verbatim, got %q", out["README.md"])
	}
}

// ageKeyService declines a non-age key type with a descriptive error so the next
// key service in the chain handles it — covering Decrypt's type-assertion guard.
func TestAgeKeyService_DeclinesNonAgeKey(t *testing.T) {
	identity, _ := newAgeKey(t)
	d, err := New(Keys{Age: []string{identity}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	svc, ok := d.keyServices[0].(*ageKeyService)
	if !ok {
		t.Fatalf("expected the first key service to be an ageKeyService, got %T", d.keyServices[0])
	}
	_, err = svc.Decrypt(context.Background(), &keyservice.DecryptRequest{
		Key:        &keyservice.Key{KeyType: &keyservice.Key_PgpKey{PgpKey: &keyservice.PgpKey{}}},
		Ciphertext: []byte("x"),
	})
	if err == nil {
		t.Fatal("the age service must decline a non-age key so the next service handles it")
	}
	if !strings.Contains(err.Error(), "only age keys are supported") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// pgpKeyService declines a non-PGP key type so the chain falls through, covering
// the type-assertion guard at the top of pgpKeyService.Decrypt.
func TestPGPKeyService_DeclinesNonPGPKey(t *testing.T) {
	_, asc := newPGPEntity(t)
	entities, err := parsePGPKeys([]string{asc})
	if err != nil {
		t.Fatalf("parsePGPKeys: %v", err)
	}
	svc := &pgpKeyService{entities: entities}
	_, err = svc.Decrypt(context.Background(), &keyservice.DecryptRequest{
		Key:        &keyservice.Key{KeyType: &keyservice.Key_AgeKey{AgeKey: &keyservice.AgeKey{Recipient: "age1xxx"}}},
		Ciphertext: []byte("x"),
	})
	if err == nil {
		t.Fatal("the pgp service must decline a non-pgp key so the next service handles it")
	}
}

// A PGP data key that is not valid armored data fails at the armor-decode step,
// covering the armor.Decode error branch of pgpKeyService.Decrypt distinct from a
// wrong-key (read-message) failure.
func TestPGPKeyService_ArmorDecodeError(t *testing.T) {
	_, asc := newPGPEntity(t)
	entities, err := parsePGPKeys([]string{asc})
	if err != nil {
		t.Fatalf("parsePGPKeys: %v", err)
	}
	svc := &pgpKeyService{entities: entities}
	_, err = svc.Decrypt(context.Background(), decryptRequest([]byte("not-armored-pgp-data")))
	if err == nil {
		t.Fatal("non-armored ciphertext must fail at the armor-decode step")
	}
	if !strings.Contains(err.Error(), "armor-decode") {
		t.Errorf("expected an armor-decode error, got: %v", err)
	}
}

// kmsKeyService.Encrypt is not supported and returns an error for every cloud key
// service's Encrypt — the decryptor never encrypts. Covers the Encrypt stubs that
// otherwise sit at 0%.
func TestKeyServices_EncryptUnsupported(t *testing.T) {
	services := []keyservice.KeyServiceClient{
		&ageKeyService{},
		&pgpKeyService{},
		&kmsKeyService{},
	}
	for _, svc := range services {
		if _, err := svc.Encrypt(context.Background(), &keyservice.EncryptRequest{}); err == nil {
			t.Errorf("%T.Encrypt must report that encryption is unsupported", svc)
		}
	}
}

// kmsContext maps a non-empty encryption-context map into the pointer-valued
// shape SOPS expects, copying each value so the returned pointers are stable and
// independent of the loop variable. Exercises the populated branch of kmsContext.
func TestKMSContext_PopulatedMap(t *testing.T) {
	in := map[string]string{"env": "prod", "team": "payments"}
	out := kmsContext(in)
	if len(out) != len(in) {
		t.Fatalf("kmsContext returned %d entries, want %d", len(out), len(in))
	}
	for k, v := range in {
		got, ok := out[k]
		if !ok {
			t.Fatalf("kmsContext dropped key %q", k)
		}
		if got == nil {
			t.Fatalf("kmsContext value for %q is nil", k)
		}
		if *got != v {
			t.Errorf("kmsContext[%q] = %q, want %q", k, *got, v)
		}
	}
	// Distinct keys must point at distinct, correct values — a guard against a
	// loop-variable aliasing bug where every pointer would observe the same value.
	if *out["env"] == *out["team"] {
		t.Error("kmsContext pointers alias the same value; each key must keep its own value")
	}
}

// kmsContext returns nil for an empty input so SOPS' AWS master key sees no
// encryption context, covering the early-return arm.
func TestKMSContext_EmptyMapReturnsNil(t *testing.T) {
	if out := kmsContext(map[string]string{}); out != nil {
		t.Errorf("kmsContext(empty) = %v, want nil", out)
	}
	if out := kmsContext(nil); out != nil {
		t.Errorf("kmsContext(nil) = %v, want nil", out)
	}
}
