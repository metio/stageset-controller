// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package rollbackstore

import (
	"context"
	"io"
	"os"
	"strings"
)

// FileStore keeps rendered run output on a filesystem directory — typically an
// RWX PersistentVolume mounted into the controller, so multiple HA replicas
// share one store (leader election serializes writes). It is the in-cluster
// alternative to the S3 backend: no object-store credentials, no Helm-style
// Secret size limit. It satisfies the controller's RollbackStore interface
// structurally.
type FileStore struct {
	root *os.Root
}

// NewFile opens (creating if needed) the store directory. All access is rooted
// there via os.Root, so a crafted key can never traverse outside it.
func NewFile(dir string) (*FileStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &FileStore{root: root}, nil
}

// flatName collapses the "/"-separated key into a single slash-free filename.
// Kubernetes names (namespace/name/stage) and the sanitized digest contain no
// "_" or "/", so the mapping is injective and yields no subdirectories — there
// is nothing to traverse.
func flatName(key string) string {
	return strings.ReplaceAll(key, "/", "_")
}

// Put writes the rendered output for a key.
func (f *FileStore) Put(_ context.Context, key string, data []byte) error {
	file, err := f.root.Create(flatName(key))
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	_, err = file.Write(data)
	return err
}

// Get returns the rendered output for a key, or (nil, false, nil) when absent.
func (f *FileStore) Get(_ context.Context, key string) ([]byte, bool, error) {
	file, err := f.root.Open(flatName(key))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, false, err
	}
	return data, true, nil
}
