// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/fluxcd/pkg/auth"
	authaws "github.com/fluxcd/pkg/auth/aws"
	authazure "github.com/fluxcd/pkg/auth/azure"
	authgcp "github.com/fluxcd/pkg/auth/gcp"
	"golang.org/x/oauth2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/metio/stageset-controller/internal/decryptor"
)

// tenantCredentialSource resolves cloud KMS credentials for the object-level-KMS
// path through fluxcd/pkg/auth, scoped to a tenant ServiceAccount. pkg/auth
// federates that ServiceAccount's projected token to the cloud's STS (IRSA /
// Workload Identity / Azure Workload Identity), so the KMS call carries the
// tenant's cloud identity — never the controller's. The returned adapters are
// lazy: they reach the cloud only when a SOPS master key's Decrypt runs.
//
// It implements decryptor.CredentialSource; production wires it, tests inject a
// fake so the resolution + wiring is exercised without a cloud account.
type tenantCredentialSource struct {
	client    client.Client
	namespace string
	saName    string
}

var _ decryptor.CredentialSource = (*tenantCredentialSource)(nil)

// opts builds the common pkg/auth options binding the credentials to the tenant
// ServiceAccount and the controller-runtime client used to read it.
func (s *tenantCredentialSource) opts() []auth.Option {
	return []auth.Option{
		auth.WithClient(s.client),
		auth.WithServiceAccountName(s.saName),
		auth.WithServiceAccountNamespace(s.namespace),
	}
}

func (s *tenantCredentialSource) AWS(ctx context.Context) awssdk.CredentialsProvider {
	return authaws.NewCredentialsProvider(ctx, s.opts()...)
}

func (s *tenantCredentialSource) GCP(ctx context.Context) oauth2.TokenSource {
	return authgcp.NewTokenSource(ctx, s.opts()...)
}

func (s *tenantCredentialSource) Azure(ctx context.Context) azcore.TokenCredential {
	return authazure.NewTokenCredential(ctx, s.opts()...)
}
