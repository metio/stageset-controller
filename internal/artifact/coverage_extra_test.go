// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package artifact

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

// --- guard.go pure-function edges ---------------------------------------------

func TestParseIPAny_CanonicalAndAtonForms(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want string // "" means nil
	}{
		{"127.0.0.1", "127.0.0.1"},     // canonical dotted-quad via net.ParseIP
		{"::1", "::1"},                 // canonical IPv6 via net.ParseIP
		{"2130706433", "127.0.0.1"},    // inet_aton single-int fallback
		{"source.flux-system.svc", ""}, // a hostname is not an IP literal
		{"", ""},                       // empty
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got := ParseIPAny(tc.in)
			if tc.want == "" {
				if got != nil {
					t.Fatalf("ParseIPAny(%q) = %v, want nil", tc.in, got)
				}
				return
			}
			if got == nil || got.String() != tc.want {
				t.Fatalf("ParseIPAny(%q) = %v, want %s", tc.in, got, tc.want)
			}
		})
	}
}

func TestPermissiveIP_AlwaysAllows(t *testing.T) {
	t.Parallel()
	for _, ip := range []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("169.254.169.254"), net.ParseIP("::1"), nil} {
		if err := PermissiveIP(ip); err != nil {
			t.Fatalf("PermissiveIP(%v) = %v, want nil", ip, err)
		}
	}
}

func TestPermissiveHTTPURL_SchemeOnlyAndErrors(t *testing.T) {
	t.Parallel()
	if err := PermissiveHTTPURL("http://127.0.0.1/x"); err != nil {
		t.Fatalf("loopback must pass the permissive guard: %v", err)
	}
	if err := PermissiveHTTPURL("https://localhost/x"); err != nil {
		t.Fatalf("localhost must pass the permissive guard: %v", err)
	}
	if err := PermissiveHTTPURL("file:///etc/passwd"); !errors.Is(err, ErrInvalidScheme) {
		t.Fatalf("non-http scheme: want ErrInvalidScheme, got %v", err)
	}
	// A control byte in the URL makes url.Parse fail, exercising the parse-error
	// branch of PermissiveHTTPURL.
	if err := PermissiveHTTPURL("http://\x7f\x00/x"); !errors.Is(err, ErrForbiddenHost) {
		t.Fatalf("unparsable url: want ErrForbiddenHost, got %v", err)
	}
}

func TestValidateHTTPURL_UnparsableURL(t *testing.T) {
	t.Parallel()
	if err := validateHTTPURL("http://\x7f\x00/x"); !errors.Is(err, ErrForbiddenHost) {
		t.Fatalf("unparsable url: want ErrForbiddenHost, got %v", err)
	}
}

func TestParseCNumber_BaseDispatch(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in      string
		want    uint64
		wantErr bool
	}{
		{"0", 0, false},     // single leading zero is plain decimal zero
		{"10", 10, false},   // decimal
		{"010", 8, false},   // octal
		{"0x1f", 31, false}, // hex lower
		{"0X1F", 31, false}, // hex upper
		{"", 0, true},       // empty
		{"08", 0, true},     // 8 is not an octal digit
		{"0xZZ", 0, true},   // not hex
		{"99zz", 0, true},   // not decimal
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got, err := parseCNumber(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseCNumber(%q) = %d, want error", tc.in, got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("parseCNumber(%q) = (%d,%v), want (%d,nil)", tc.in, got, err, tc.want)
			}
		})
	}
}

func TestParseInetAtonIPv4_ExtraEdges(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want string // "" means nil
	}{
		{"1.2.3.4", "1.2.3.4"}, // full four parts, each a single byte
		{"0", "0.0.0.0"},       // a single zero is the all-zero address
		{"0.0.0.256", ""},      // final single-byte part out of range
		{"0xffffffff", "255.255.255.255"},
		{"0x100000000", ""}, // overflows the 32-bit address space
		{"1.0x1000000", ""}, // two parts: final slot holds 3 bytes (max 0xffffff), 0x1000000 exceeds it
		{"256.0.0.0.0", ""}, // more than four parts
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got := parseInetAtonIPv4(tc.in)
			if tc.want == "" {
				if got != nil {
					t.Fatalf("parseInetAtonIPv4(%q) = %v, want nil", tc.in, got)
				}
				return
			}
			if got == nil || got.String() != tc.want {
				t.Fatalf("parseInetAtonIPv4(%q) = %v, want %s", tc.in, got, tc.want)
			}
		})
	}
}

func TestForbiddenIP_MulticastAndUnspecified(t *testing.T) {
	t.Parallel()
	if forbiddenIP(net.ParseIP("224.0.0.1")) == nil {
		t.Fatal("multicast must be rejected")
	}
	if forbiddenIP(net.ParseIP("ff02::1")) == nil {
		t.Fatal("IPv6 link-local multicast must be rejected")
	}
	if forbiddenIP(net.ParseIP("0.0.0.0")) == nil {
		t.Fatal("unspecified must be rejected")
	}
	if forbiddenIP(net.ParseIP("8.8.8.8")) != nil {
		t.Fatal("a routable public address must be permitted")
	}
}

// --- fetcher.go edges ---------------------------------------------------------

func TestVerifyDigest_TrimsAndCases(t *testing.T) {
	t.Parallel()
	const got = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	// Surrounding whitespace and mixed case in the expectation are normalised.
	if err := verifyDigest(got, "  SHA256:"+strings.ToUpper(got)+"  "); err != nil {
		t.Fatalf("trim+lowercase normalisation failed: %v", err)
	}
	// No colon at all.
	if err := verifyDigest(got, "sha256"); !errors.Is(err, ErrDigestInvalid) {
		t.Fatalf("missing colon: want ErrDigestInvalid, got %v", err)
	}
	// Colon at the very end (empty hex).
	if err := verifyDigest(got, "sha256:"); !errors.Is(err, ErrDigestInvalid) {
		t.Fatalf("trailing colon: want ErrDigestInvalid, got %v", err)
	}
	// Leading colon (empty algorithm).
	if err := verifyDigest(got, ":"+got); !errors.Is(err, ErrDigestInvalid) {
		t.Fatalf("empty algo: want ErrDigestInvalid, got %v", err)
	}
	// Correct length but a non-hex byte.
	bad := strings.Repeat("g", len(got))
	if err := verifyDigest(got, "sha256:"+bad); !errors.Is(err, ErrDigestInvalid) {
		t.Fatalf("non-hex digest: want ErrDigestInvalid, got %v", err)
	}
}

func TestIsHex(t *testing.T) {
	t.Parallel()
	if !isHex("0123456789abcdef") {
		t.Fatal("all hex digits should be hex")
	}
	if isHex("ABCDEF") {
		t.Fatal("uppercase is not accepted (digests are lowercased before this check)")
	}
	if isHex("xyz") {
		t.Fatal("letters past f are not hex")
	}
	if !isHex("") {
		t.Fatal("empty string trivially contains only hex bytes")
	}
}

func TestFetcher_HTTPClientDefault(t *testing.T) {
	t.Parallel()
	// An empty Fetcher falls back to http.DefaultClient.
	if (&Fetcher{}).httpClient() != http.DefaultClient {
		t.Fatal("nil HTTPClient should fall back to http.DefaultClient")
	}
	custom := &http.Client{}
	if (&Fetcher{HTTPClient: custom}).httpClient() != custom {
		t.Fatal("set HTTPClient should be returned verbatim")
	}
}

func TestFetcher_IPValidatorDefault(t *testing.T) {
	t.Parallel()
	// A Fetcher with no IPValidator falls back to forbiddenIP, which rejects loopback.
	if err := (&Fetcher{}).ipValidator()(net.ParseIP("127.0.0.1")); err == nil {
		t.Fatal("default ipValidator must reject loopback")
	}
	// An explicit validator is honoured.
	if err := (&Fetcher{IPValidator: PermissiveIP}).ipValidator()(net.ParseIP("127.0.0.1")); err != nil {
		t.Fatalf("explicit PermissiveIP should allow loopback: %v", err)
	}
}

func TestFetch_RequestBuildError(t *testing.T) {
	t.Parallel()
	// A method-illegal control byte in the URL passes the permissive URL guard
	// but makes http.NewRequestWithContext fail, exercising downloadToTemp's
	// request-build error branch.
	f := testFetcher()
	_, err := f.Fetch(context.Background(), "http://example.com/\x7f", sha256Digest([]byte("x")), "")
	if err == nil {
		t.Fatal("a control byte in the URL should fail request construction")
	}
}

func TestFetch_BadGzipStream(t *testing.T) {
	t.Parallel()
	// A body whose digest matches but is not a gzip stream trips gzip.NewReader.
	body := []byte("this is plainly not gzip")
	srv := serveBytes(t, body, 0)
	_, err := testFetcher().Fetch(context.Background(), srv.URL, sha256Digest(body), "")
	if err == nil || !strings.Contains(err.Error(), "open gzip stream") {
		t.Fatalf("want gzip-open error, got %v", err)
	}
}

func TestFetch_NonRegularEntriesSkipped(t *testing.T) {
	t.Parallel()
	// A directory entry and a symlink entry are non-regular and silently skipped;
	// only the regular file survives.
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	mustWrite := func(h *tar.Header, body string) {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if body != "" {
			if _, err := tw.Write([]byte(body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	mustWrite(&tar.Header{Name: "dir/", Typeflag: tar.TypeDir, Mode: 0o755}, "")
	mustWrite(&tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "dir/a.yaml", Mode: 0o777}, "")
	mustWrite(&tar.Header{Name: "dir/a.yaml", Typeflag: tar.TypeReg, Size: 4, Mode: 0o644}, "kind")
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	body := buf.Bytes()
	srv := serveBytes(t, body, 0)

	files, err := testFetcher().Fetch(context.Background(), srv.URL, sha256Digest(body), "")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(files) != 1 || files["dir/a.yaml"] != "kind" {
		t.Fatalf("only the regular file should survive, got %#v", files)
	}
}

func TestFetch_OutOfPrefixEntrySkipped(t *testing.T) {
	t.Parallel()
	// An entry whose name fails normaliseEntry's prefix filter is dropped, and an
	// unsafe-byte name is dropped too, leaving only the in-prefix safe file.
	tarball := makeTarGz(t, map[string]string{
		"keep/a.yaml":  "A",
		"drop/b.yaml":  "B",
		"keep/bad c.y": "C", // space is not a safe path byte
	})
	srv := serveBytes(t, tarball, 0)
	files, err := testFetcher().Fetch(context.Background(), srv.URL, sha256Digest(tarball), "keep")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(files) != 1 || files["a.yaml"] != "A" {
		t.Fatalf("prefix filter should keep only keep/a.yaml, got %#v", files)
	}
}

func TestFetch_DecompressedStreamCapExceeded(t *testing.T) {
	t.Parallel()
	// A tar with a large file inflates well past a tiny MaxDecompressedBytes, so
	// the cappedReader trips before the per-entry/extracted caps. The compressed
	// body stays small, so MaxArchiveBytes must be generous.
	tarball := makeTarGz(t, map[string]string{"a.yaml": strings.Repeat("q", 5000)})
	srv := serveBytes(t, tarball, 0)
	f := testFetcher()
	f.MaxArchiveBytes = 1 << 20
	f.MaxDecompressedBytes = 64 // far below the 5000-byte inflated stream
	_, err := f.Fetch(context.Background(), srv.URL, sha256Digest(tarball), "")
	if !errors.Is(err, ErrDecompressedTooLarge) {
		t.Fatalf("want ErrDecompressedTooLarge, got %v", err)
	}
}

func TestFetch_HeaderSizeOverExtractedCapPrecheck(t *testing.T) {
	t.Parallel()
	// Two entries: the first fills the extracted budget, the second's header size
	// exceeds the remaining budget and trips the aggregate precheck before the
	// body is read.
	tarball := makeTarGz(t, map[string]string{
		"a.yaml": strings.Repeat("x", 80),
		"b.yaml": strings.Repeat("y", 80),
	})
	srv := serveBytes(t, tarball, 0)
	f := testFetcher()
	f.MaxArchiveBytes = 1 << 20
	f.MaxPerEntryBytes = 1 << 20
	f.MaxDecompressedBytes = 1 << 20
	f.MaxExtractedBytes = 100 // admits one 80-byte file, not two
	_, err := f.Fetch(context.Background(), srv.URL, sha256Digest(tarball), "")
	if !errors.Is(err, ErrTarballTooLarge) {
		t.Fatalf("want ErrTarballTooLarge, got %v", err)
	}
}

// --- safeDialContext / production New() path ----------------------------------

func TestSafeDialContext_RejectsForbiddenResolvedIP(t *testing.T) {
	t.Parallel()
	// A real loopback listener whose URL host is "localhost" passes the permissive
	// URL guard (string level) but the production IP validator pins the resolved
	// 127.0.0.1 and rejects the dial, exercising safeDialContext's reject branch.
	tarball := makeTarGz(t, map[string]string{"a.yaml": "x"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(tarball)
	}))
	t.Cleanup(srv.Close)

	f := New()
	f.URLValidator = PermissiveHTTPURL // let the loopback host reach the dialer
	url := strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)
	_, err := f.Fetch(context.Background(), url, sha256Digest(tarball), "")
	if !errors.Is(err, ErrForbiddenAddress) {
		t.Fatalf("want ErrForbiddenAddress from the dial-time pin, got %v", err)
	}
}

func TestSafeDialContext_AllowsAndDialsWithPermissiveIP(t *testing.T) {
	t.Parallel()
	// With PermissiveIP the dialer's check passes and it dials the resolved
	// loopback address — the happy path through safeDialContext.
	tarball := makeTarGz(t, map[string]string{"a.yaml": "kind: A"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(tarball)
	}))
	t.Cleanup(srv.Close)

	f := New()
	f.URLValidator = PermissiveHTTPURL
	f.IPValidator = PermissiveIP
	url := strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)
	files, err := f.Fetch(context.Background(), url, sha256Digest(tarball), "")
	if err != nil {
		t.Fatalf("permissive dial should succeed: %v", err)
	}
	if files["a.yaml"] != "kind: A" {
		t.Fatalf("unexpected files: %#v", files)
	}
}

func TestSafeDialContext_BadHostPort(t *testing.T) {
	t.Parallel()
	// SplitHostPort fails on an address with no port, surfacing as a dial error.
	f := New()
	_, err := f.safeDialContext(context.Background(), "tcp", "no-port-here")
	if err == nil {
		t.Fatal("a portless address should fail SplitHostPort")
	}
}

func TestSafeDialContext_UnresolvableHost(t *testing.T) {
	t.Parallel()
	// A syntactically valid but non-resolving name fails the LookupIPAddr step.
	f := New()
	_, err := f.safeDialContext(context.Background(), "tcp", "this-host-does-not-exist.invalid:443")
	if err == nil {
		t.Fatal("an unresolvable host should fail the lookup")
	}
}

// --- resolver.go edges --------------------------------------------------------

func TestReadyState_Branches(t *testing.T) {
	t.Parallel()
	// No status.conditions at all.
	if ok, why := readyState(&unstructured.Unstructured{Object: map[string]any{}}); ok || why == "" {
		t.Fatalf("missing conditions: got (%v,%q)", ok, why)
	}

	mk := func(conds []any) *unstructured.Unstructured {
		u := &unstructured.Unstructured{Object: map[string]any{}}
		if err := unstructured.SetNestedSlice(u.Object, conds, "status", "conditions"); err != nil {
			t.Fatal(err)
		}
		return u
	}

	// A non-map element is skipped; a non-Ready type is skipped; then no Ready.
	if ok, why := readyState(mk([]any{"not-a-map", map[string]any{"type": "Reconciling", "status": "True"}})); ok || why != "no Ready condition" {
		t.Fatalf("non-Ready conditions: got (%v,%q)", ok, why)
	}

	// Ready=False carries the reason in the message.
	ok, why := readyState(mk([]any{map[string]any{"type": "Ready", "status": "False", "reason": "BuildFailed"}}))
	if ok || !strings.Contains(why, "BuildFailed") {
		t.Fatalf("Ready=False: got (%v,%q)", ok, why)
	}

	// Ready=True.
	if ok, _ := readyState(mk([]any{map[string]any{"type": "Ready", "status": "True"}})); !ok {
		t.Fatal("Ready=True should report ready")
	}
}

func TestReadArtifact_EmptyAndMissing(t *testing.T) {
	t.Parallel()
	mk := func(art map[string]any) *unstructured.Unstructured {
		u := &unstructured.Unstructured{Object: map[string]any{}}
		if art != nil {
			if err := unstructured.SetNestedMap(u.Object, art, "status", "artifact"); err != nil {
				t.Fatal(err)
			}
		}
		return u
	}

	// No status.artifact.
	if _, err := readArtifact(mk(nil)); !errors.Is(err, ErrArtifactMissing) {
		t.Fatalf("missing artifact: want ErrArtifactMissing, got %v", err)
	}
	// Empty url.
	if _, err := readArtifact(mk(map[string]any{"digest": "sha256:abc"})); !errors.Is(err, ErrArtifactMissing) {
		t.Fatalf("empty url: want ErrArtifactMissing, got %v", err)
	}
	// url set, empty digest.
	if _, err := readArtifact(mk(map[string]any{"url": "http://x/y.tar.gz"})); !errors.Is(err, ErrArtifactMissing) {
		t.Fatalf("empty digest: want ErrArtifactMissing, got %v", err)
	}
	// Full artifact, no revision — revision stays empty, no error.
	got, err := readArtifact(mk(map[string]any{"url": "http://x/y.tar.gz", "digest": "sha256:abc"}))
	if err != nil {
		t.Fatalf("full artifact: %v", err)
	}
	if got.URL == "" || got.Digest == "" || got.Revision != "" {
		t.Fatalf("unexpected artifact: %#v", got)
	}
}

func TestVerifiedState_NonMapElementSkipped(t *testing.T) {
	t.Parallel()
	u := &unstructured.Unstructured{Object: map[string]any{}}
	if err := unstructured.SetNestedSlice(u.Object, []any{
		"not-a-map",
		map[string]any{"type": "SourceVerified", "status": "True"},
	}, "status", "conditions"); err != nil {
		t.Fatal(err)
	}
	if v := verifiedState(u); v == nil || !*v {
		t.Fatalf("non-map element should be skipped and SourceVerified=True honoured, got %v", v)
	}
}

func TestResolveProducer_SkipsEntriesWithoutSourceRef(t *testing.T) {
	t.Parallel()
	// One EA carries no spec.sourceRef (skipped), the other back-references the
	// producer and is the unique match.
	noRef := externalArtifactFixture("ns", "orphan", nil, true, readyArtifact())
	match := externalArtifactFixture("ns", "the-one", snippetBackPointer("dashboards"), true, readyArtifact())
	c := buildClient(t, noRef, match)
	ref := stagesv1.SourceReference{APIVersion: "jaas.metio.wtf/v1", Kind: "JsonnetSnippet", Name: "dashboards"}
	got, err := (&Resolver{}).Resolve(context.Background(), c, ref, "ns")
	if err != nil {
		t.Fatalf("Resolve producer: %v", err)
	}
	if got.Key() != "ns/the-one" {
		t.Fatalf("want ns/the-one, got %q", got.Key())
	}
}

func TestGetDirectSource_NonNotFoundGetError(t *testing.T) {
	t.Parallel()
	// An interceptor surfaces a non-NotFound Get error, which getDirectSource must
	// wrap and return verbatim (not collapse to ErrArtifactNotFound).
	sentinel := errors.New("apiserver exploded")
	base := fake.NewClientBuilder().WithScheme(resolverScheme(t)).Build()
	c := interceptor.NewClient(base, interceptor.Funcs{
		Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
			return sentinel
		},
	})
	_, err := (&Resolver{}).Resolve(context.Background(), c, stagesv1.SourceReference{Name: "art1"}, "ns")
	if !errors.Is(err, sentinel) {
		t.Fatalf("non-NotFound Get error should propagate, got %v", err)
	}
	if errors.Is(err, ErrArtifactNotFound) {
		t.Fatalf("non-NotFound error must not become ErrArtifactNotFound: %v", err)
	}
}

func TestResolveProducer_ListError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("list blew up")
	base := fake.NewClientBuilder().WithScheme(resolverScheme(t)).Build()
	c := interceptor.NewClient(base, interceptor.Funcs{
		List: func(_ context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
			return sentinel
		},
	})
	ref := stagesv1.SourceReference{APIVersion: "jaas.metio.wtf/v1", Kind: "JsonnetSnippet", Name: "dashboards"}
	_, err := (&Resolver{}).Resolve(context.Background(), c, ref, "ns")
	if !errors.Is(err, sentinel) {
		t.Fatalf("List error should propagate, got %v", err)
	}
}
