// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package rollbackstore

import (
	"context"
	"testing"
)

func TestFileStore_PutGetRoundTrip(t *testing.T) {
	t.Parallel()
	store, err := NewFile(t.TempDir())
	if err != nil {
		t.Fatalf("NewFile: %v", err)
	}
	ctx := context.Background()
	key := "ns/app/stage-a/sha256-deadbeef"
	want := []byte(`[{"kind":"ConfigMap"}]`)

	if err := store.Put(ctx, key, want); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, found, err := store.Get(ctx, key)
	if err != nil || !found {
		t.Fatalf("Get: found=%v err=%v", found, err)
	}
	if string(got) != string(want) {
		t.Fatalf("round-trip mismatch: got %q want %q", got, want)
	}
}

func TestFileStore_GetMissReturnsNotFound(t *testing.T) {
	t.Parallel()
	store, err := NewFile(t.TempDir())
	if err != nil {
		t.Fatalf("NewFile: %v", err)
	}
	if data, found, err := store.Get(context.Background(), "ns/app/stage-a/absent"); err != nil || found || data != nil {
		t.Fatalf("missing key should be (nil,false,nil), got (%v,%v,%v)", data, found, err)
	}
}

// A key overwrite replaces the stored content (last write wins).
func TestFileStore_PutOverwrites(t *testing.T) {
	t.Parallel()
	store, err := NewFile(t.TempDir())
	if err != nil {
		t.Fatalf("NewFile: %v", err)
	}
	ctx := context.Background()
	key := "ns/app/stage-a/rev"
	if err := store.Put(ctx, key, []byte("old")); err != nil {
		t.Fatalf("Put old: %v", err)
	}
	if err := store.Put(ctx, key, []byte("new")); err != nil {
		t.Fatalf("Put new: %v", err)
	}
	got, _, _ := store.Get(ctx, key)
	if string(got) != "new" {
		t.Fatalf("overwrite: got %q want new", got)
	}
}
