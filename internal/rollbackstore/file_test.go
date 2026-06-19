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

// TestFileStore_PartialWriteNotVisible proves the atomic-write invariant: a temp
// file left behind by a crash mid-write (staged but never renamed) is never
// returned by Get as a valid snapshot. Only the renamed destination counts.
func TestFileStore_PartialWriteNotVisible(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := NewFile(dir)
	if err != nil {
		t.Fatalf("NewFile: %v", err)
	}
	ctx := context.Background()
	key := "ns/app/stage-a/rev"

	// Simulate a crash mid-Put: the staged temp file exists, the destination
	// does not (the rename never happened).
	tmp := flatName(key) + ".tmp"
	f, err := store.root.Create(tmp)
	if err != nil {
		t.Fatalf("stage temp: %v", err)
	}
	if _, err := f.Write([]byte("truncated-and-partial")); err != nil {
		t.Fatalf("write temp: %v", err)
	}
	_ = f.Close()

	if data, found, err := store.Get(ctx, key); err != nil || found || data != nil {
		t.Fatalf("a partial write must be invisible to Get, got (%q,%v,%v)", data, found, err)
	}

	// A subsequent successful Put still commits, and replaces (does not append
	// to) the stale temp file.
	want := []byte(`[{"kind":"Secret"}]`)
	if err := store.Put(ctx, key, want); err != nil {
		t.Fatalf("Put after stale temp: %v", err)
	}
	got, found, err := store.Get(ctx, key)
	if err != nil || !found || string(got) != string(want) {
		t.Fatalf("Put after stale temp: got (%q,%v,%v) want %q", got, found, err, want)
	}
}

// TestFileStore_FailedPutKeepsPreviousValue proves a Put that errors mid-flight
// leaves the previously committed snapshot intact (no truncation of the live
// destination).
func TestFileStore_FailedPutKeepsPreviousValue(t *testing.T) {
	t.Parallel()
	store, err := NewFile(t.TempDir())
	if err != nil {
		t.Fatalf("NewFile: %v", err)
	}
	ctx := context.Background()
	key := "ns/app/stage-a/rev"
	good := []byte("good-snapshot")
	if err := store.Put(ctx, key, good); err != nil {
		t.Fatalf("Put good: %v", err)
	}

	// Occupy the temp name with a NON-EMPTY directory so the next Put can neither
	// Remove nor Create over it, and so fails before it can touch the live
	// destination.
	tmp := flatName(key) + ".tmp"
	if err := store.root.Mkdir(tmp, 0o700); err != nil {
		t.Fatalf("mkdir temp blocker: %v", err)
	}
	if c, cerr := store.root.Create(tmp + "/child"); cerr != nil {
		t.Fatalf("populate temp blocker: %v", cerr)
	} else {
		_ = c.Close()
	}
	if err := store.Put(ctx, key, []byte("doomed")); err == nil {
		t.Fatal("Put should fail when the temp name is blocked")
	}

	got, found, err := store.Get(ctx, key)
	if err != nil || !found || string(got) != string(good) {
		t.Fatalf("failed Put must keep the previous value: got (%q,%v,%v) want %q", got, found, err, good)
	}
}
