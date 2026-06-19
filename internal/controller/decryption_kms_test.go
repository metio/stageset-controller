// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/getsops/sops/v3"
	"github.com/getsops/sops/v3/cmd/sops/common"
	"github.com/getsops/sops/v3/cmd/sops/formats"
	"github.com/getsops/sops/v3/config"
	sopskms "github.com/getsops/sops/v3/kms"
	"golang.org/x/oauth2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

// recordingCredentialSource is the account-free seam double for the controller
// tests: it records whether the per-tenant cloud credential resolution was
// reached and returns inert adapters, so the object-level-KMS plumbing is
// asserted without any cloud call.
type recordingCredentialSource struct{ consulted int }

func (s *recordingCredentialSource) AWS(context.Context) awssdk.CredentialsProvider {
	s.consulted++
	return awssdk.CredentialsProviderFunc(func(context.Context) (awssdk.Credentials, error) {
		return awssdk.Credentials{}, errTestNoCloud
	})
}

func (s *recordingCredentialSource) GCP(context.Context) oauth2.TokenSource {
	s.consulted++
	return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "fake"})
}

func (s *recordingCredentialSource) Azure(context.Context) azcore.TokenCredential {
	s.consulted++
	return inertAzureCred{}
}

type errString string

func (e errString) Error() string { return string(e) }

const errTestNoCloud = errString("no cloud in tests")

type inertAzureCred struct{}

func (inertAzureCred) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{}, errTestNoCloud
}

// kmsSOPSFile hand-builds a SOPS YAML whose only key group is an AWS KMS key.
// No cloud is touched to construct it: the metadata carries an arbitrary
// encrypted data key, enough that DecryptTree routes the data-key decryption
// through the KMS key service. The decrypt itself fails (no cloud); the test
// only asserts which credential path was selected on the way there.
func kmsSOPSFile(t *testing.T) string {
	t.Helper()
	store := common.StoreForFormat(formats.Yaml, config.NewStoresConfig())
	branches, err := store.LoadPlainFile([]byte("kind: ConfigMap\ndata:\n  k: v\n"))
	if err != nil {
		t.Fatalf("load plain: %v", err)
	}
	mk := sopskms.NewMasterKeyFromArn("arn:aws:kms:us-east-1:111122223333:key/abc", nil, "")
	mk.EncryptedKey = "AQICAHhfake"
	tree := sops.Tree{
		Branches: branches,
		Metadata: sops.Metadata{
			Version:                   "3.13.1",
			MessageAuthenticationCode: "ENC[AES256_GCM,data:fake,iv:fake,tag:fake,type:str]",
			KeyGroups:                 []sops.KeyGroup{{mk}},
		},
	}
	out, err := store.EmitEncryptedFile(tree)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	return string(out)
}

// Object-level KMS ON with a serviceAccountName resolves the per-tenant
// credential source and hands it to the KMS key service. Asserted via the seam:
// decrypting a KMS file consults the recording source. Account-free — the cloud
// call after resolution fails and is irrelevant to this assertion.
func TestBuildDecryptor_ObjectLevelKMS_On_UsesPerTenantSource(t *testing.T) {
	const ns = "team-a"
	r := builderWith(t)
	r.SkipImpersonation = true // local client without an apiserver mint
	r.ObjectLevelKMS = true
	rec := &recordingCredentialSource{}
	r.credentialSource = rec

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "ss"},
		Spec: stagesv1.StageSetSpec{
			ServiceAccountName: "tenant-sa",
			Decryption:         &stagesv1.Decryption{Provider: "sops"},
		},
	}
	d, err := r.buildDecryptor(context.Background(), ss)
	if err != nil {
		t.Fatalf("buildDecryptor: %v", err)
	}
	// The decrypt fails for lack of a real cloud; the contract under test is that
	// the per-tenant source was consulted on the KMS path.
	_, _ = d.DecryptFiles(map[string]string{"secret.yaml": kmsSOPSFile(t)})
	if rec.consulted == 0 {
		t.Fatal("object-level KMS on: the per-tenant credential source was never consulted")
	}
}

// Object-level KMS OFF keeps the ambient path: the per-tenant credential source
// is never wired, so decrypting a KMS file never consults it (the controller's
// ambient credentials handle KMS instead).
func TestBuildDecryptor_ObjectLevelKMS_Off_IsAmbient(t *testing.T) {
	const ns = "team-a"
	r := builderWith(t)
	r.SkipImpersonation = true
	r.ObjectLevelKMS = false
	rec := &recordingCredentialSource{}
	r.credentialSource = rec

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "ss"},
		Spec: stagesv1.StageSetSpec{
			ServiceAccountName: "tenant-sa",
			Decryption:         &stagesv1.Decryption{Provider: "sops"},
		},
	}
	d, err := r.buildDecryptor(context.Background(), ss)
	if err != nil {
		t.Fatalf("buildDecryptor: %v", err)
	}
	_, _ = d.DecryptFiles(map[string]string{"secret.yaml": kmsSOPSFile(t)})
	if rec.consulted != 0 {
		t.Fatalf("object-level KMS off: per-tenant source consulted %d times, want 0 (ambient path)", rec.consulted)
	}
}

// Object-level KMS ON but no serviceAccountName falls back to the ambient path:
// there is no tenant identity to federate, so the per-tenant source is not wired.
func TestBuildDecryptor_ObjectLevelKMS_On_NoSA_FallsBackToAmbient(t *testing.T) {
	const ns = "team-a"
	r := builderWith(t)
	r.SkipImpersonation = true
	r.ObjectLevelKMS = true
	rec := &recordingCredentialSource{}
	r.credentialSource = rec

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "ss"},
		Spec: stagesv1.StageSetSpec{
			Decryption: &stagesv1.Decryption{Provider: "sops"},
		},
	}
	d, err := r.buildDecryptor(context.Background(), ss)
	if err != nil {
		t.Fatalf("buildDecryptor: %v", err)
	}
	_, _ = d.DecryptFiles(map[string]string{"secret.yaml": kmsSOPSFile(t)})
	if rec.consulted != 0 {
		t.Fatalf("no serviceAccountName: per-tenant source consulted %d times, want 0", rec.consulted)
	}
}
