// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package artifact

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// fatalHelper is the minimal failure surface both *testing.T and *rapid.T
// expose, so a fixture builder can serve ordinary and property tests alike.
type fatalHelper interface {
	Helper()
	Fatalf(format string, args ...any)
}

// tarGzTB builds a gzip+tar archive against the minimal failure surface, so both
// ordinary tests and rapid property tests can build fixtures. It mirrors
// makeTarGz, which is pinned to *testing.T.
func tarGzTB(t fatalHelper, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header: %v", err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("write body: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return buf.Bytes()
}

// serveBytesRapid serves body over a loopback httptest server, closing it via
// the rapid run's Cleanup. (rapid.T has its own Cleanup, distinct from the
// testing.TB surface, so this can't share serveBytes.)
func serveBytesRapid(rt *rapid.T, body []byte) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	rt.Cleanup(srv.Close)
	return srv
}

// TestFetch_DecompressedCapExceeded pins the gzip-bomb defence directly: a
// highly compressible entry whose inflated size dwarfs both the compressed body
// and the decompressed cap is rejected with ErrDecompressedTooLarge, before the
// per-entry or aggregate caps would notice. The caps are arranged so only the
// decompressed-stream cap can fire (per-entry and archive caps stay generous).
func TestFetch_DecompressedCapExceeded(t *testing.T) {
	t.Parallel()
	// 1 MiB of zero bytes compresses to a few KiB, so the archive body stays
	// tiny while the decompressed stream blows past the 4 KiB cap.
	bomb := strings.Repeat("\x00", 1<<20)
	tarball := makeTarGz(t, map[string]string{"bomb.yaml": bomb})
	srv := serveBytes(t, tarball, 0)

	f := testFetcher()
	f.MaxArchiveBytes = 1 << 20      // generous: the compressed body is far smaller
	f.MaxPerEntryBytes = 2 << 20     // generous: would admit the entry on size alone
	f.MaxDecompressedBytes = 4 << 10 // the only cap small enough to fire
	_, err := f.Fetch(context.Background(), srv.URL, sha256Digest(tarball), "")
	if !errors.Is(err, ErrDecompressedTooLarge) {
		t.Fatalf("a gzip bomb must trip the decompressed-stream cap; got %v", err)
	}
}

// TestFetch_PerEntryCap_Property generates a single-entry archive of a random
// size against a random per-entry cap (archive + decompressed caps held
// generous) and asserts the per-entry cap is the deciding policy: oversize
// entries are rejected with ErrTarEntryTooLarge, within-cap entries round-trip.
func TestFetch_PerEntryCap_Property(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(rt *rapid.T) {
		entrySize := rapid.IntRange(1, 8000).Draw(rt, "entrySize")
		content := string(rapid.SliceOfN(rapid.Byte(), entrySize, entrySize).Draw(rt, "content"))
		tarball := tarGzTB(rt, map[string]string{"data.yaml": content})

		perEntryCap := int64(rapid.IntRange(1, 8000).Draw(rt, "perEntryCap"))
		srv := serveBytesRapid(rt, tarball)
		f := testFetcher()
		f.MaxArchiveBytes = 1 << 20
		f.MaxDecompressedBytes = 1 << 20
		f.MaxPerEntryBytes = perEntryCap

		files, err := f.Fetch(context.Background(), srv.URL, sha256Digest(tarball), "")
		if int64(entrySize) > perEntryCap {
			if !errors.Is(err, ErrTarEntryTooLarge) {
				rt.Fatalf("entry %d > perEntry %d must be rejected with ErrTarEntryTooLarge, got %v", entrySize, perEntryCap, err)
			}
			return
		}
		if err != nil {
			rt.Fatalf("entry %d <= perEntry %d should be accepted, got %v", entrySize, perEntryCap, err)
		}
		if files["data.yaml"] != content {
			rt.Fatalf("content did not round-trip")
		}
	})
}

// TestFetch_ArchiveBodyCap_Property pins the compressed-download cap: a served
// body above MaxArchiveBytes is rejected at download time with
// ErrArtifactBodyTooLarge. The extracted-result cap is held generous so only the
// compressed cap can fire; a body under the cap is accepted.
func TestFetch_ArchiveBodyCap_Property(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(rt *rapid.T) {
		size := rapid.IntRange(1, 6000).Draw(rt, "entrySize")
		content := string(rapid.SliceOfN(rapid.Byte(), size, size).Draw(rt, "content"))
		tarball := tarGzTB(rt, map[string]string{"data.yaml": content})

		archiveCap := int64(rapid.IntRange(1, 8000).Draw(rt, "archiveCap"))
		srv := serveBytesRapid(rt, tarball)
		f := testFetcher()
		f.MaxPerEntryBytes = 1 << 20
		f.MaxDecompressedBytes = 1 << 20
		f.MaxExtractedBytes = 1 << 20
		f.MaxArchiveBytes = archiveCap

		_, err := f.Fetch(context.Background(), srv.URL, sha256Digest(tarball), "")
		switch {
		case int64(len(tarball)) > archiveCap:
			if !errors.Is(err, ErrArtifactBodyTooLarge) {
				rt.Fatalf("compressed body %d > cap %d must trip ErrArtifactBodyTooLarge, got %v", len(tarball), archiveCap, err)
			}
		default:
			if err != nil {
				rt.Fatalf("compressed body %d <= cap %d should be accepted, got %v", len(tarball), archiveCap, err)
			}
		}
	})
}

// TestFetch_ExtractedCap_Property pins the extracted-result cap, independent of
// the compressed-download cap. The compressed body fits (archive + decompressed
// caps held generous) but the extracted total is measured against
// MaxExtractedBytes: a total over the cap is rejected at extraction time with
// ErrTarballTooLarge, otherwise the content round-trips.
func TestFetch_ExtractedCap_Property(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(rt *rapid.T) {
		size := rapid.IntRange(1, 6000).Draw(rt, "entrySize")
		content := string(rapid.SliceOfN(rapid.Byte(), size, size).Draw(rt, "content"))
		tarball := tarGzTB(rt, map[string]string{"data.yaml": content})

		extractedCap := int64(rapid.IntRange(1, 8000).Draw(rt, "extractedCap"))
		srv := serveBytesRapid(rt, tarball)
		f := testFetcher()
		f.MaxArchiveBytes = 1 << 20
		f.MaxPerEntryBytes = 1 << 20
		f.MaxDecompressedBytes = 1 << 20
		f.MaxExtractedBytes = extractedCap

		files, err := f.Fetch(context.Background(), srv.URL, sha256Digest(tarball), "")
		if int64(size) > extractedCap {
			if !errors.Is(err, ErrTarballTooLarge) {
				rt.Fatalf("extracted total %d > cap %d must trip ErrTarballTooLarge, got %v", size, extractedCap, err)
			}
			return
		}
		if err != nil {
			rt.Fatalf("extracted total %d <= cap %d should be accepted, got %v", size, extractedCap, err)
		}
		if files["data.yaml"] != content {
			rt.Fatalf("content did not round-trip")
		}
	})
}
