// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

// Package rollbackstore provides external object-store backends for the
// rollbackOnFailure feature: the rendered output of successful runs is pushed
// to a store the controller owns, so rollback is bit-exact and independent of
// the producer's artifact retention. It is the deliberate alternative to
// Helm-style in-Secret release storage — the store has no 1 MiB object limit.
package rollbackstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/minio-go/v7/pkg/encrypt"
)

// S3Config configures the S3-compatible rollback store.
type S3Config struct {
	Endpoint     string
	Bucket       string
	Prefix       string
	Region       string
	UseSSL       bool
	AccessKey    string
	SecretKey    string
	SessionToken string
	// Anonymous uses unsigned requests (public buckets); otherwise empty
	// static credentials engage minio-go's IAM/IRSA discovery chain.
	Anonymous bool
	// SSE selects server-side encryption at rest for every stored object:
	// "none" (no SSE header), "s3" (SSE-S3, the bucket's managed key), or "kms"
	// (SSE-KMS with SSEKMSKeyID). The rollback store holds rendered Secret
	// data, so the chart defaults this on; only buckets whose backend cannot
	// honor an SSE header (some self-hosted S3-compatibles) need "none".
	SSE string
	// SSEKMSKeyID is the KMS key ARN/ID for SSE="kms".
	SSEKMSKeyID string
}

// S3Store stores rendered run output in any S3-compatible bucket. It satisfies
// the controller's RollbackStore interface structurally.
type S3Store struct {
	client *minio.Client
	bucket string
	prefix string
	sse    encrypt.ServerSide
}

// s3Credentials selects the minio credential provider from the config:
// anonymous → nil (unsigned), an explicit access key → static V4, otherwise the
// discovery chain (env → shared file → EC2/EKS IAM). AWS_* environment variables
// and IRSA web-identity tokens are honored through the env entry, so a Secret of
// AWS_* keys mounted via envFrom authenticates here.
func s3Credentials(cfg S3Config) *credentials.Credentials {
	switch {
	case cfg.Anonymous:
		return nil
	case cfg.AccessKey != "":
		return credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, cfg.SessionToken)
	default:
		// Discovery chain: env vars first, then EC2/EKS metadata.
		// IRSA web-identity tokens are honored via the env chain.
		return credentials.NewChainCredentials([]credentials.Provider{
			&credentials.EnvAWS{},
			&credentials.FileAWSCredentials{},
			&credentials.IAM{Client: &http.Client{Timeout: 5 * time.Second}},
		})
	}
}

// NewS3 builds an S3 rollback store. Empty static credentials fall through to
// minio-go's environment / EC2 / EKS (IRSA) credential chain.
func NewS3(cfg S3Config) (*S3Store, error) {
	sse, err := serverSideEncryption(cfg.SSE, cfg.SSEKMSKeyID)
	if err != nil {
		return nil, err
	}
	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  s3Credentials(cfg),
		Secure: cfg.UseSSL,
		Region: cfg.Region,
	})
	if err != nil {
		return nil, err
	}
	return &S3Store{client: client, bucket: cfg.Bucket, prefix: cfg.Prefix, sse: sse}, nil
}

// serverSideEncryption maps the SSE mode string onto a minio encrypt.ServerSide.
// "kms" applies whether or not a key is named: an empty SSEKMSKeyID selects the
// bucket's default KMS key (AWS resolves aws/s3), so it is not an error.
func serverSideEncryption(mode, kmsKeyID string) (encrypt.ServerSide, error) {
	switch mode {
	case "", "none":
		return nil, nil
	case "s3":
		return encrypt.NewSSE(), nil
	case "kms":
		sse, err := encrypt.NewSSEKMS(kmsKeyID, nil)
		if err != nil {
			return nil, fmt.Errorf("building SSE-KMS config: %w", err)
		}
		return sse, nil
	default:
		return nil, fmt.Errorf("unknown S3 SSE mode %q (want none, s3, or kms)", mode)
	}
}

func (s *S3Store) objectName(key string) string {
	if s.prefix == "" {
		return key
	}
	return strings.TrimRight(s.prefix, "/") + "/" + key
}

// Put writes the rendered output for a key.
func (s *S3Store) Put(ctx context.Context, key string, data []byte) error {
	_, err := s.client.PutObject(ctx, s.bucket, s.objectName(key),
		bytes.NewReader(data), int64(len(data)),
		minio.PutObjectOptions{ContentType: "application/json", ServerSideEncryption: s.sse})
	return err
}

// Get returns the rendered output for a key, or (nil, false, nil) when absent.
func (s *S3Store) Get(ctx context.Context, key string) ([]byte, bool, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, s.objectName(key), minio.GetObjectOptions{})
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = obj.Close() }()
	data, err := io.ReadAll(obj)
	if err != nil {
		// GetObject is lazy; a missing object surfaces here as NoSuchKey.
		var resp minio.ErrorResponse
		if errors.As(err, &resp) && resp.Code == "NoSuchKey" {
			return nil, false, nil
		}
		return nil, false, err
	}
	return data, true, nil
}
