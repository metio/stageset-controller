// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package rollbackstore

import (
	"context"
	"fmt"
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

// Put writes the rendered output for a key atomically: it stages the bytes in a
// sibling temp file, fsyncs, then renames over the destination. A crash or a
// disk-full mid-write leaves only the temp file — never a truncated destination
// that Get would return as a valid (but partial) snapshot. os.Root scopes both
// the temp file and the rename inside the store directory.
func (f *FileStore) Put(_ context.Context, key string, data []byte) error {
	final := flatName(key)
	tmp := final + ".tmp"
	// A stale temp file from a previous crash must not block the create.
	_ = f.root.Remove(tmp)
	file, err := f.root.Create(tmp)
	if err != nil {
		return err
	}
	// Remove the temp file on any failure path so a partial write never lingers.
	committed := false
	defer func() {
		_ = file.Close()
		if !committed {
			_ = f.root.Remove(tmp)
		}
	}()
	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("write rollback snapshot: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync rollback snapshot: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close rollback snapshot: %w", err)
	}
	if err := f.root.Rename(tmp, final); err != nil {
		return fmt.Errorf("commit rollback snapshot: %w", err)
	}
	committed = true
	return nil
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
	data, err := readCapped(file)
	if err != nil {
		return nil, false, err
	}
	return data, true, nil
}
