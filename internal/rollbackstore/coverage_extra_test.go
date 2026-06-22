// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package rollbackstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Key composes the canonical "<ns>/<name>/<stage>/<digest>" address with ':' in
// the digest rewritten to '-' so it is a safe single path/object segment.
func TestKey_ComposesAddressAndSanitizesDigestColon(t *testing.T) {
	t.Parallel()
	got := Key("ns", "app", "stage-a", "sha256:deadbeef")
	want := "ns/app/stage-a/sha256-deadbeef"
	if got != want {
		t.Fatalf("Key = %q, want %q", got, want)
	}
	// A digest with multiple colons has every colon rewritten.
	if got := Key("ns", "app", "s", "a:b:c"); got != "ns/app/s/a-b-c" {
		t.Fatalf("Key colon rewrite = %q, want ns/app/s/a-b-c", got)
	}
}

// The composed Key round-trips through a FileStore: a value written under the
// Key is returned for the same Key.
func TestKey_RoundTripsThroughFileStore(t *testing.T) {
	t.Parallel()
	store, err := NewFile(t.TempDir())
	if err != nil {
		t.Fatalf("NewFile: %v", err)
	}
	ctx := context.Background()
	key := Key("ns", "app", "stage-a", "sha256:abc123")
	want := []byte("manifest")
	if err := store.Put(ctx, key, want); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, found, err := store.Get(ctx, key)
	if err != nil || !found || !bytes.Equal(got, want) {
		t.Fatalf("Get = (%q,%v,%v), want (%q,true,nil)", got, found, err, want)
	}
}

// NewFile fails when the target path cannot be a directory — here a regular file
// already occupies it, so MkdirAll errors before os.OpenRoot is reached.
func TestNewFile_ErrorsWhenPathIsAFile(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	occupied := filepath.Join(base, "not-a-dir")
	if err := os.WriteFile(occupied, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	if _, err := NewFile(occupied); err == nil {
		t.Fatal("NewFile over a regular file should fail")
	}
}

// readCapped propagates a read error from the underlying reader rather than
// masking it as a short (but successful) read.
func TestReadCapped_PropagatesReaderError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("boom")
	if _, err := readCapped(errReader{err: sentinel}); !errors.Is(err, sentinel) {
		t.Fatalf("readCapped error = %v, want %v", err, sentinel)
	}
}

// readCapped returns exactly the bytes for content at the cap boundary minus
// one, confirming the +1 over-read does not corrupt the returned slice.
func TestReadCapped_AtAndBelowBoundary(t *testing.T) {
	t.Parallel()
	for _, n := range []int{0, 1, 1024} {
		data := bytes.Repeat([]byte("z"), n)
		got, err := readCapped(bytes.NewReader(data))
		if err != nil || len(got) != n {
			t.Fatalf("readCapped(%d) = (%d bytes, %v), want (%d, nil)", n, len(got), err, n)
		}
	}
}

// errReader always fails on Read with a fixed error.
type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

// FileStore.Get rejects an object that exceeds MaxObjectBytes — a planted
// oversized snapshot on a shared volume must not be loaded into memory whole.
func TestFileStore_GetRejectsOversizedObject(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := NewFile(dir)
	if err != nil {
		t.Fatalf("NewFile: %v", err)
	}
	key := "ns/app/stage/over"
	// Write past the cap directly through os.Root, bypassing Put's data path.
	f, err := store.root.Create(flatName(key))
	if err != nil {
		t.Fatalf("create oversized: %v", err)
	}
	if _, err := f.Write(make([]byte, MaxObjectBytes+1)); err != nil {
		t.Fatalf("write oversized: %v", err)
	}
	_ = f.Close()

	if _, _, err := store.Get(context.Background(), key); err == nil {
		t.Fatal("Get of an oversized object should error")
	}
}

// NewS3 builds a usable client for a well-formed endpoint (the success path that
// the bad-SSE rejection test does not exercise).
func TestNewS3_BuildsClientForValidEndpoint(t *testing.T) {
	t.Parallel()
	store, err := NewS3(S3Config{Endpoint: "s3.example.com", Bucket: "b", Prefix: "p", SSE: "none"})
	if err != nil {
		t.Fatalf("NewS3: %v", err)
	}
	if store.bucket != "b" || store.prefix != "p" || store.client == nil {
		t.Fatalf("NewS3 store = %+v, want bucket=b prefix=p with client", store)
	}
}

// NewS3 surfaces minio.New's validation error for a malformed endpoint.
func TestNewS3_ErrorsOnMalformedEndpoint(t *testing.T) {
	t.Parallel()
	if _, err := NewS3(S3Config{Endpoint: "http://bad endpoint with spaces", Bucket: "b"}); err == nil {
		t.Fatal("NewS3 should reject a malformed endpoint")
	}
}

// newS3TestStore points an S3Store at an httptest server that emulates just
// enough of the S3 object API for Put/Get round-trips. Objects are kept in an
// in-memory map keyed by request path.
func newS3TestStore(t *testing.T, prefix string) (*S3Store, *fakeS3) {
	t.Helper()
	fake := &fakeS3{objects: map[string][]byte{}}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	endpoint := strings.TrimPrefix(srv.URL, "http://")
	store, err := NewS3(S3Config{
		Endpoint:  endpoint,
		Bucket:    "bucket",
		Prefix:    prefix,
		Region:    "us-east-1",
		UseSSL:    false,
		AccessKey: "AKIA",
		SecretKey: "secret",
		SSE:       "none",
	})
	if err != nil {
		t.Fatalf("NewS3: %v", err)
	}
	return store, fake
}

// fakeS3 is a minimal path-style S3 object store backed by an in-memory map. It
// answers the PUT/GET object operations the S3Store exercises and returns a
// NoSuchKey error document for a missing object.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
}

// lastModified is a fixed RFC1123 timestamp the fake stamps on object reads;
// minio-go parses this header and rejects a response that omits it.
const lastModified = "Mon, 02 Jan 2006 15:04:05 GMT"

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Path-style addressing: /<bucket>/<object...>. A bucket-only request with a
	// "location" query is the region probe minio issues before object calls.
	if _, ok := r.URL.Query()["location"]; ok {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/">us-east-1</LocationConstraint>`)
		return
	}
	key := objectKeyFromPath(r.URL)
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.Method {
	case http.MethodPut:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		// minio signs the upload with AWS streaming-chunked encoding; recover the
		// raw object bytes from the chunk framing before storing.
		if strings.HasPrefix(r.Header.Get("X-Amz-Content-Sha256"), "STREAMING-") {
			body = dechunk(body)
		}
		f.objects[key] = body
		w.Header().Set("ETag", `"deadbeef"`)
		w.WriteHeader(http.StatusOK)
	case http.MethodHead:
		data, ok := f.objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", itoa(len(data)))
		w.Header().Set("Last-Modified", lastModified)
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		data, ok := f.objects[key]
		if !ok {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>NoSuchKey</Code><Message>The specified key does not exist.</Message></Error>`)
			return
		}
		w.Header().Set("Content-Length", itoa(len(data)))
		w.Header().Set("Last-Modified", lastModified)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// dechunk strips AWS streaming-signature chunk framing
// ("<hexlen>;chunk-signature=<sig>\r\n<data>\r\n…0;chunk-signature=…\r\n\r\n")
// down to the concatenated payload bytes.
func dechunk(body []byte) []byte {
	var out []byte
	for {
		nl := bytes.IndexByte(body, '\n')
		if nl < 0 {
			break
		}
		header := strings.TrimRight(string(body[:nl]), "\r")
		body = body[nl+1:]
		sizeHex := header
		if i := strings.IndexByte(header, ';'); i >= 0 {
			sizeHex = header[:i]
		}
		size := parseHex(sizeHex)
		if size == 0 {
			break
		}
		if size > len(body) {
			size = len(body)
		}
		out = append(out, body[:size]...)
		body = body[size:]
		// Drop the trailing CRLF after each chunk's data.
		if len(body) >= 2 && body[0] == '\r' && body[1] == '\n' {
			body = body[2:]
		}
	}
	return out
}

// parseHex parses a lowercase/uppercase hexadecimal chunk size, returning 0 on
// any unexpected character.
func parseHex(s string) int {
	n := 0
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			n = n*16 + int(r-'0')
		case r >= 'a' && r <= 'f':
			n = n*16 + int(r-'a'+10)
		case r >= 'A' && r <= 'F':
			n = n*16 + int(r-'A'+10)
		default:
			return 0
		}
	}
	return n
}

// objectKeyFromPath strips the leading "/<bucket>/" prefix to recover the
// object name the client addressed.
func objectKeyFromPath(u *url.URL) string {
	p := strings.TrimPrefix(u.EscapedPath(), "/")
	if i := strings.IndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return ""
}

// itoa renders a small non-negative int without importing strconv at call sites.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// S3Store.Put followed by Get returns the stored bytes, exercising the object
// round-trip against the fake S3 server (including the prefix join in
// objectName).
func TestS3Store_PutGetRoundTrip(t *testing.T) {
	t.Parallel()
	store, fake := newS3TestStore(t, "rollbacks")
	ctx := context.Background()
	key := Key("ns", "app", "stage-a", "sha256:abc123")
	want := []byte(`[{"kind":"ConfigMap"}]`)

	if err := store.Put(ctx, key, want); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// The object lands under the configured prefix.
	if _, ok := fake.objects["rollbacks/"+key]; !ok {
		t.Fatalf("object not stored under prefixed key; have %v", keysOf(fake.objects))
	}
	got, found, err := store.Get(ctx, key)
	if err != nil || !found || !bytes.Equal(got, want) {
		t.Fatalf("Get = (%q,%v,%v), want (%q,true,nil)", got, found, err, want)
	}
}

// S3Store.Get reports a missing object as (nil,false,nil) by recognizing the
// NoSuchKey error document the lazy GetObject surfaces on first read.
func TestS3Store_GetMissReturnsNotFound(t *testing.T) {
	t.Parallel()
	store, _ := newS3TestStore(t, "")
	data, found, err := store.Get(context.Background(), "ns/app/stage/absent")
	if err != nil || found || data != nil {
		t.Fatalf("missing object = (%v,%v,%v), want (nil,false,nil)", data, found, err)
	}
}

// keysOf returns the keys of an object map for diagnostic messages.
func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
