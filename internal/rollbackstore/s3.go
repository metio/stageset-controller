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
	"io"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
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
}

// S3Store stores rendered run output in any S3-compatible bucket. It satisfies
// the controller's RollbackStore interface structurally.
type S3Store struct {
	client *minio.Client
	bucket string
	prefix string
}

// NewS3 builds an S3 rollback store. Empty static credentials fall through to
// minio-go's environment / EC2 / EKS (IRSA) credential chain.
func NewS3(cfg S3Config) (*S3Store, error) {
	var creds *credentials.Credentials
	switch {
	case cfg.Anonymous:
		creds = credentials.NewStaticV4("", "", "")
	case cfg.AccessKey != "":
		creds = credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, cfg.SessionToken)
	default:
		creds = credentials.NewIAM("")
	}
	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  creds,
		Secure: cfg.UseSSL,
		Region: cfg.Region,
	})
	if err != nil {
		return nil, err
	}
	return &S3Store{client: client, bucket: cfg.Bucket, prefix: cfg.Prefix}, nil
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
		minio.PutObjectOptions{ContentType: "application/json"})
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
