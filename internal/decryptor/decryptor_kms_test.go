// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package decryptor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/getsops/sops/v3/keyservice"
	"golang.org/x/oauth2"
)

// fakeCredentialSource is the account-free test double for CredentialSource. It
// records which cloud was asked for credentials and returns inert adapters, so a
// test can assert the per-tenant resolution + wiring without any cloud call. The
// adapters are never driven to the cloud here: the assertions stop at "the
// source was consulted".
type fakeCredentialSource struct {
	awsCalls   int
	gcpCalls   int
	azureCalls int
}

func (f *fakeCredentialSource) AWS(context.Context) awssdk.CredentialsProvider {
	f.awsCalls++
	return awssdk.CredentialsProviderFunc(func(context.Context) (awssdk.Credentials, error) {
		return awssdk.Credentials{}, errNoCloud
	})
}

func (f *fakeCredentialSource) GCP(context.Context) oauth2.TokenSource {
	f.gcpCalls++
	return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "fake"})
}

func (f *fakeCredentialSource) Azure(context.Context) azcore.TokenCredential {
	f.azureCalls++
	return fakeAzureCred{}
}

var errNoCloud = errAdapter("no cloud in tests")

type errAdapter string

func (e errAdapter) Error() string { return string(e) }

type fakeAzureCred struct{}

func (fakeAzureCred) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{}, errNoCloud
}

// New with a CredentialSource keeps age + PGP decryption working: the per-tenant
// kms service declines non-KMS key types, and DecryptTree falls through to the
// in-process age service. This is the account-free guard that the opt-in does
// not regress local decryption.
func TestNew_WithCredentialSource_AgeStillDecrypts(t *testing.T) {
	identity, recipient := newAgeKey(t)
	encrypted := sopsEncryptYAML(t, recipient, plainSecret)

	fake := &fakeCredentialSource{}
	d, err := New(Keys{Age: []string{identity}}, WithCredentialSource(fake))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	out, err := d.DecryptFiles(map[string]string{"secret.yaml": encrypted})
	if err != nil {
		t.Fatalf("DecryptFiles: %v", err)
	}
	if !strings.Contains(out["secret.yaml"], "supersecret") {
		t.Fatalf("age decryption regressed under object-level KMS, got:\n%s", out["secret.yaml"])
	}
	// An age file's data key never reaches the cloud KMS service.
	if fake.awsCalls+fake.gcpCalls+fake.azureCalls != 0 {
		t.Errorf("credential source consulted for an age key: aws=%d gcp=%d azure=%d",
			fake.awsCalls, fake.gcpCalls, fake.azureCalls)
	}
}

// WithCredentialSource(nil) is a no-op: the option threads unconditionally and a
// nil source keeps the ambient default (no kms service wired). Asserted via the
// seam — a nil source means no credential resolution can happen.
func TestNew_NilCredentialSource_IsAmbient(t *testing.T) {
	identity, recipient := newAgeKey(t)
	encrypted := sopsEncryptYAML(t, recipient, plainSecret)
	d, err := New(Keys{Age: []string{identity}}, WithCredentialSource(nil))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if hasKMSKeyService(d) {
		t.Fatal("a nil credential source must not wire the per-tenant kms service")
	}
	out, err := d.DecryptFiles(map[string]string{"secret.yaml": encrypted})
	if err != nil {
		t.Fatalf("DecryptFiles: %v", err)
	}
	if !strings.Contains(out["secret.yaml"], "supersecret") {
		t.Fatal("age decryption must still work on the ambient path")
	}
}

// The default New (no option) selects the ambient path: only the local client
// follows the in-process age/PGP services, never the per-tenant kms service.
func TestNew_Default_SelectsAmbientPath(t *testing.T) {
	d, err := New(Keys{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if hasKMSKeyService(d) {
		t.Fatal("the default decryptor must not wire the per-tenant kms service (ambient KMS is the default)")
	}
}

// Object-level KMS ON resolves the per-tenant credential source and hands it to
// the KMS master key construction. Driving kmsKeyService.Decrypt with an AWS KMS
// request asserts the fake source's AWS() was consulted before any cloud call —
// the account-free half of the contract. The subsequent mk.Decrypt fails for
// lack of a real cloud (errNoCloud), which is expected and cloud-CI-only.
func TestKMSKeyService_ResolvesPerTenantCredentials(t *testing.T) {
	tests := []struct {
		name string
		key  *keyservice.Key
		want func(*fakeCredentialSource) int
	}{
		{
			name: "aws",
			key:  &keyservice.Key{KeyType: &keyservice.Key_KmsKey{KmsKey: &keyservice.KmsKey{Arn: "arn:aws:kms:us-east-1:111122223333:key/abc"}}},
			want: func(f *fakeCredentialSource) int { return f.awsCalls },
		},
		{
			name: "gcp",
			key:  &keyservice.Key{KeyType: &keyservice.Key_GcpKmsKey{GcpKmsKey: &keyservice.GcpKmsKey{ResourceId: "projects/p/locations/l/keyRings/r/cryptoKeys/k"}}},
			want: func(f *fakeCredentialSource) int { return f.gcpCalls },
		},
		{
			name: "azure",
			key:  &keyservice.Key{KeyType: &keyservice.Key_AzureKeyvaultKey{AzureKeyvaultKey: &keyservice.AzureKeyVaultKey{VaultUrl: "https://v.vault.azure.net", Name: "k", Version: "1"}}},
			want: func(f *fakeCredentialSource) int { return f.azureCalls },
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeCredentialSource{}
			svc := &kmsKeyService{creds: fake}
			// The cloud call is expected to fail (no account); we only assert
			// the per-tenant source was consulted on the way to the KMS client.
			// A short deadline bounds the doomed cloud attempt so the SDK's
			// default retry budget doesn't slow the account-free assertion.
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			_, _ = svc.Decrypt(ctx, &keyservice.DecryptRequest{
				Key:        tc.key,
				Ciphertext: []byte("ciphertext"),
			})
			if got := tc.want(fake); got != 1 {
				t.Fatalf("%s: credential source consulted %d times, want exactly 1", tc.name, got)
			}
		})
	}
}

// A key type the per-tenant kms service does not own (e.g. age) returns an error
// so DecryptTree falls through to the next key service, preserving non-cloud-KMS
// behavior. No credential source is consulted.
func TestKMSKeyService_DeclinesNonKMSKeys(t *testing.T) {
	fake := &fakeCredentialSource{}
	svc := &kmsKeyService{creds: fake}
	_, err := svc.Decrypt(context.Background(), &keyservice.DecryptRequest{
		Key:        &keyservice.Key{KeyType: &keyservice.Key_AgeKey{AgeKey: &keyservice.AgeKey{Recipient: "age1xxx"}}},
		Ciphertext: []byte("x"),
	})
	if err == nil {
		t.Fatal("the kms service must decline an age key so the next service handles it")
	}
	if fake.awsCalls+fake.gcpCalls+fake.azureCalls != 0 {
		t.Error("credential source must not be consulted for a key type the kms service declines")
	}
}

// hasKMSKeyService reports whether d wired the per-tenant kms service — the
// observable difference between the object-level and ambient paths.
func hasKMSKeyService(d *Decryptor) bool {
	for _, s := range d.keyServices {
		if _, ok := s.(*kmsKeyService); ok {
			return true
		}
	}
	return false
}
