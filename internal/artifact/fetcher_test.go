// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package artifact

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// New must build its transport from DefaultTransport (so it inherits
// TLSHandshakeTimeout and friends) with the proxy cleared (so a configured
// HTTP(S)_PROXY can't route the dial around the IP-pinning defence).
func TestNew_TransportClonedWithoutProxy(t *testing.T) {
	f := New()
	tr, ok := f.HTTPClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport = %T, want *http.Transport", f.HTTPClient.Transport)
	}
	if tr.Proxy != nil {
		t.Error("transport.Proxy must be nil so dialing isn't routed through a proxy")
	}
	if tr.TLSHandshakeTimeout == 0 {
		t.Error("transport should inherit DefaultTransport's TLSHandshakeTimeout (zero means a fresh, untuned transport)")
	}
}

func makeTarGz(t *testing.T, files map[string]string) []byte {
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

func sha256Digest(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func testFetcher() *Fetcher {
	// A vanilla client (no IP-pinning dialer) plus the permissive URL guard so
	// httptest loopback listeners are reachable.
	return &Fetcher{HTTPClient: http.DefaultClient, URLValidator: PermissiveHTTPURL, IPValidator: PermissiveIP}
}

func serveBytes(t *testing.T, body []byte, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFetch_RejectsDuplicateNormalizedEntries(t *testing.T) {
	t.Parallel()
	// "a/b.yaml" and "a/./b.yaml" both normalize to "a/b.yaml"; without a guard
	// the second silently shadows the first and changes what gets applied.
	tarball := makeTarGz(t, map[string]string{"a/b.yaml": "kind: A", "a/./b.yaml": "kind: B"})
	srv := serveBytes(t, tarball, 0)
	if _, err := testFetcher().Fetch(context.Background(), srv.URL, sha256Digest(tarball), ""); !errors.Is(err, ErrDuplicateEntry) {
		t.Fatalf("duplicate normalized entries: err = %v, want ErrDuplicateEntry", err)
	}
}

func TestFetch_MultiMemberGzipExtractsAllFiles(t *testing.T) {
	t.Parallel()
	// One tar gzipped across TWO members (a producer that flushes mid-stream, or
	// member concatenation of a single logical tar). With multistream on the
	// members decompress transparently as one stream, so every file must be
	// extracted — not just the first member's. Goes through Fetch's *os.File path.
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	for name, content := range map[string]string{"a/one.yaml": "kind: A", "a/two.yaml": "kind: B"} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	raw := tarBuf.Bytes()
	mid := len(raw) / 2

	// Two independent gzip members appended to one buffer.
	var out bytes.Buffer
	for _, chunk := range [][]byte{raw[:mid], raw[mid:]} {
		gz := gzip.NewWriter(&out)
		if _, err := gz.Write(chunk); err != nil {
			t.Fatal(err)
		}
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
	}
	body := out.Bytes()
	srv := serveBytes(t, body, 0)

	files, err := testFetcher().Fetch(context.Background(), srv.URL, sha256Digest(body), "")
	if err != nil {
		t.Fatalf("multi-member single-tar Fetch: %v", err)
	}
	if files["a/one.yaml"] != "kind: A" || files["a/two.yaml"] != "kind: B" {
		t.Fatalf("multi-member gzip dropped files: %#v", files)
	}
}

func TestFetch_HappyPath(t *testing.T) {
	t.Parallel()
	tarball := makeTarGz(t, map[string]string{"a/main.yaml": "kind: A", "a/sub/b.yaml": "kind: B"})
	srv := serveBytes(t, tarball, 0)

	files, err := testFetcher().Fetch(context.Background(), srv.URL, sha256Digest(tarball), "")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if files["a/main.yaml"] != "kind: A" || files["a/sub/b.yaml"] != "kind: B" {
		t.Fatalf("unexpected files: %#v", files)
	}
}

func TestFetch_PathPrefixFilter(t *testing.T) {
	t.Parallel()
	tarball := makeTarGz(t, map[string]string{"a/main.yaml": "A", "b/other.yaml": "B"})
	srv := serveBytes(t, tarball, 0)

	files, err := testFetcher().Fetch(context.Background(), srv.URL, sha256Digest(tarball), "a")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if _, ok := files["b/other.yaml"]; ok {
		t.Fatalf("prefix filter leaked an out-of-prefix entry: %#v", files)
	}
	if files["main.yaml"] != "A" {
		t.Fatalf("want main.yaml (prefix stripped), got %#v", files)
	}
}

func TestFetch_DigestMismatch(t *testing.T) {
	t.Parallel()
	tarball := makeTarGz(t, map[string]string{"a.yaml": "x"})
	srv := serveBytes(t, tarball, 0)

	_, err := testFetcher().Fetch(context.Background(), srv.URL, sha256Digest([]byte("different")), "")
	if !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("want ErrDigestMismatch, got %v", err)
	}
}

func TestFetch_InvalidDigest(t *testing.T) {
	t.Parallel()
	tarball := makeTarGz(t, map[string]string{"a.yaml": "x"})
	srv := serveBytes(t, tarball, 0)

	_, err := testFetcher().Fetch(context.Background(), srv.URL, "not-a-digest", "")
	if !errors.Is(err, ErrDigestInvalid) {
		t.Fatalf("want ErrDigestInvalid, got %v", err)
	}
}

func TestFetch_HTTPNotFound(t *testing.T) {
	t.Parallel()
	srv := serveBytes(t, nil, http.StatusNotFound)
	_, err := testFetcher().Fetch(context.Background(), srv.URL, sha256Digest([]byte("x")), "")
	if err == nil {
		t.Fatal("want error on HTTP 404")
	}
}

func TestFetch_PerEntryCapExceeded(t *testing.T) {
	t.Parallel()
	tarball := makeTarGz(t, map[string]string{"big.yaml": strings.Repeat("x", 2000)})
	srv := serveBytes(t, tarball, 0)

	f := testFetcher()
	f.MaxPerEntryBytes = 1000
	_, err := f.Fetch(context.Background(), srv.URL, sha256Digest(tarball), "")
	if !errors.Is(err, ErrTarEntryTooLarge) {
		t.Fatalf("want ErrTarEntryTooLarge, got %v", err)
	}
}

func TestFetch_ArchiveBodyCapExceeded(t *testing.T) {
	t.Parallel()
	tarball := makeTarGz(t, map[string]string{"a.yaml": strings.Repeat("y", 4000)})
	srv := serveBytes(t, tarball, 0)

	f := testFetcher()
	f.MaxArchiveBytes = 16 // smaller than the served body
	_, err := f.Fetch(context.Background(), srv.URL, sha256Digest(tarball), "")
	if !errors.Is(err, ErrArtifactBodyTooLarge) {
		t.Fatalf("want ErrArtifactBodyTooLarge, got %v", err)
	}
}

// TestFetch_ArchiveAndExtractedCapsIndependent proves the compressed-download
// cap and the extracted-result cap are two independent knobs. A body that fits
// the (generous) compressed cap but whose extracted total exceeds a small
// MaxExtractedBytes trips ErrTarballTooLarge; the inverse — a body over a small
// MaxArchiveBytes with a generous extracted cap — trips ErrArtifactBodyTooLarge.
func TestFetch_ArchiveAndExtractedCapsIndependent(t *testing.T) {
	t.Parallel()
	tarball := makeTarGz(t, map[string]string{"a.yaml": strings.Repeat("z", 4000)})

	t.Run("extracted cap fires while archive cap is generous", func(t *testing.T) {
		t.Parallel()
		srv := serveBytes(t, tarball, 0)
		f := testFetcher()
		f.MaxArchiveBytes = 1 << 20      // generous: admits the small compressed body
		f.MaxPerEntryBytes = 1 << 20     // generous
		f.MaxDecompressedBytes = 1 << 20 // generous
		f.MaxExtractedBytes = 100        // the only cap small enough to fire
		_, err := f.Fetch(context.Background(), srv.URL, sha256Digest(tarball), "")
		if !errors.Is(err, ErrTarballTooLarge) {
			t.Fatalf("want ErrTarballTooLarge, got %v", err)
		}
	})

	t.Run("archive cap fires while extracted cap is generous", func(t *testing.T) {
		t.Parallel()
		srv := serveBytes(t, tarball, 0)
		f := testFetcher()
		f.MaxArchiveBytes = 16        // smaller than the served compressed body
		f.MaxExtractedBytes = 1 << 20 // generous: would admit the extracted total
		_, err := f.Fetch(context.Background(), srv.URL, sha256Digest(tarball), "")
		if !errors.Is(err, ErrArtifactBodyTooLarge) {
			t.Fatalf("want ErrArtifactBodyTooLarge, got %v", err)
		}
	})
}

func TestFetch_RejectsForbiddenURL(t *testing.T) {
	t.Parallel()
	// The production constructor wires the URL guard, which rejects loopback
	// literals before any dial.
	f := New()
	_, err := f.Fetch(context.Background(), "http://127.0.0.1:1/x.tar.gz", sha256Digest([]byte("x")), "")
	if !errors.Is(err, ErrForbiddenHost) {
		t.Fatalf("want ErrForbiddenHost, got %v", err)
	}
}
