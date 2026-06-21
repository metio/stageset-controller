// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package artifact

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path"
	"strings"
	"time"
)

// Default size caps mirror source-controller's contract and JaaS's fetcher:
// they bound memory and defend against gzip bombs and lying tar headers.
const (
	defaultMaxArchiveBytes      int64 = 64 << 20  // compressed download only
	defaultMaxPerEntryBytes     int64 = 16 << 20  // one tar entry
	defaultMaxDecompressedBytes int64 = 512 << 20 // total inflated stream
	defaultMaxExtractedBytes    int64 = 64 << 20  // extracted result held in memory
)

// Fetch-time sentinels.
var (
	ErrDigestInvalid        = errors.New("artifact digest is not a valid <algo>:<hex> string")
	ErrDigestMismatch       = errors.New("tarball digest does not match the artifact digest")
	ErrArtifactBodyTooLarge = errors.New("artifact body exceeded the compressed-download cap")
	ErrTarballTooLarge      = errors.New("extracted tarball exceeded the extracted-result cap")
	ErrTarEntryTooLarge     = errors.New("tar entry exceeded the per-entry cap")
	ErrDecompressedTooLarge = errors.New("decompressed gzip stream exceeded the cap")
	ErrForbiddenAddress     = errors.New("artifact host resolves to a forbidden address")
)

// Fetcher downloads and extracts an ExternalArtifact tarball.
//
// URLValidator and IPValidator are injectable so tests can reach httptest
// listeners on loopback; production defaults reject loopback/link-local/
// multicast/unspecified targets while allowing the private ranges an
// in-cluster source-controller serves from.
type Fetcher struct {
	HTTPClient *http.Client
	// MaxArchiveBytes bounds ONLY the compressed download body.
	MaxArchiveBytes  int64
	MaxPerEntryBytes int64
	// MaxDecompressedBytes bounds the inflated gzip stream as it is read.
	MaxDecompressedBytes int64
	// MaxExtractedBytes bounds the total extracted result held in memory.
	MaxExtractedBytes int64
	URLValidator      func(string) error
	IPValidator       func(net.IP) error
}

// New returns a Fetcher with production defaults: the standard size caps and
// an HTTP client whose dialer pins each resolved IP through IPValidator
// (rejecting on the initial dial and every redirect hop).
func New() *Fetcher {
	f := &Fetcher{
		MaxArchiveBytes:      defaultMaxArchiveBytes,
		MaxPerEntryBytes:     defaultMaxPerEntryBytes,
		MaxDecompressedBytes: defaultMaxDecompressedBytes,
		MaxExtractedBytes:    defaultMaxExtractedBytes,
		URLValidator:         validateHTTPURL,
		IPValidator:          forbiddenIP,
	}
	f.HTTPClient = &http.Client{
		Timeout: 5 * time.Minute,
		Transport: &http.Transport{
			DialContext: f.safeDialContext,
		},
	}
	return f
}

// Fetch downloads url, verifies its sha256 against expectedDigest, and
// extracts the gzip+tar payload into a path->content map filtered by
// pathPrefix (empty prefix extracts everything). All four byte caps are
// enforced: compressed download (MaxArchiveBytes), inflated gzip stream
// (MaxDecompressedBytes), per tar entry (MaxPerEntryBytes), and the total
// extracted result held in memory (MaxExtractedBytes).
func (f *Fetcher) Fetch(ctx context.Context, url, expectedDigest, pathPrefix string) (map[string]string, error) {
	if v := f.urlValidator(); v != nil {
		if err := v(url); err != nil {
			return nil, fmt.Errorf("validate artifact url: %w", err)
		}
	}

	tmp, gotHex, err := f.downloadToTemp(ctx, url)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}()

	if err := verifyDigest(gotHex, expectedDigest); err != nil {
		return nil, err
	}

	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("rewind artifact tempfile: %w", err)
	}
	return f.extract(tmp, pathPrefix)
}

func (f *Fetcher) downloadToTemp(ctx context.Context, url string) (*os.File, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", fmt.Errorf("build artifact request: %w", err)
	}
	resp, err := f.httpClient().Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("fetch artifact: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("fetch artifact: unexpected status %d", resp.StatusCode)
	}

	tmp, err := os.CreateTemp("", "stageset-artifact-*.tar.gz")
	if err != nil {
		return nil, "", fmt.Errorf("create artifact tempfile: %w", err)
	}
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}

	hasher := sha256.New()
	maxBytes := f.maxArchiveBytes()
	// Read one byte past the cap so an oversize body is detected rather than
	// silently truncated.
	limited := io.LimitReader(resp.Body, maxBytes+1)
	written, err := io.Copy(io.MultiWriter(tmp, hasher), limited)
	if err != nil {
		cleanup()
		return nil, "", fmt.Errorf("buffer artifact: %w", err)
	}
	if written > maxBytes {
		cleanup()
		return nil, "", fmt.Errorf("%w: %d bytes", ErrArtifactBodyTooLarge, maxBytes)
	}
	return tmp, hex.EncodeToString(hasher.Sum(nil)), nil
}

func (f *Fetcher) extract(r io.Reader, pathPrefix string) (map[string]string, error) {
	// Multistream stays ON (the default): a single tar gzipped across several
	// members (a producer that flushes mid-stream) decompresses transparently as
	// one stream, so every file is extracted. Two *separate* concatenated tars
	// are unreachable past archive/tar's first end-of-archive marker regardless
	// of gzip framing, and no producer emits that shape; the digest pins the
	// bytes, so there's nothing to gain by rejecting trailing members.
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("open gzip stream: %w", err)
	}
	defer func() { _ = gz.Close() }()

	capped := &cappedReader{r: gz, remaining: f.maxDecompressedBytes()}
	tr := tar.NewReader(capped)

	prefix := path.Clean(strings.TrimSuffix(pathPrefix, "/"))
	if prefix == "." || prefix == "/" {
		prefix = ""
	}

	files := map[string]string{}
	var total int64
	perEntry := f.maxPerEntryBytes()
	maxExtracted := f.maxExtractedBytes()
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if errors.Is(err, errDecompressedCapped) {
				return nil, fmt.Errorf("%w: %d bytes", ErrDecompressedTooLarge, f.maxDecompressedBytes())
			}
			return nil, fmt.Errorf("read tar: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		name, ok := normaliseEntry(hdr.Name, prefix)
		if !ok {
			continue
		}
		if hdr.Size < 0 {
			return nil, fmt.Errorf("tar entry %q: negative size", hdr.Name)
		}
		if hdr.Size > perEntry {
			return nil, fmt.Errorf("%w: %q header size %d > %d", ErrTarEntryTooLarge, hdr.Name, hdr.Size, perEntry)
		}
		// Aggregate precheck before the read so a header near math.MaxInt64 can't
		// wrap the int64 accumulator; maxExtracted-total is non-negative (every
		// prior iteration kept total <= maxExtracted) and hdr.Size is non-negative.
		if hdr.Size > maxExtracted-total {
			return nil, fmt.Errorf("%w: %d bytes", ErrTarballTooLarge, maxExtracted)
		}
		// Bound the read at perEntry+1 to catch headers that lie about size.
		body, err := io.ReadAll(io.LimitReader(tr, perEntry+1))
		if err != nil {
			if errors.Is(err, errDecompressedCapped) {
				return nil, fmt.Errorf("%w: %d bytes", ErrDecompressedTooLarge, f.maxDecompressedBytes())
			}
			return nil, fmt.Errorf("read tar entry %q: %w", hdr.Name, err)
		}
		if int64(len(body)) > perEntry {
			return nil, fmt.Errorf("%w: %q body > %d", ErrTarEntryTooLarge, hdr.Name, perEntry)
		}
		total += int64(len(body))
		if total > maxExtracted {
			return nil, fmt.Errorf("%w: %d bytes", ErrTarballTooLarge, maxExtracted)
		}
		files[name] = string(body)
	}
	return files, nil
}

// normaliseEntry validates a tar entry path and applies the prefix filter.
// It rejects absolute paths, backslashes, NUL, "..", dot-prefixed segments,
// and any byte outside [A-Za-z0-9._/-]. The returned name is relative to
// prefix.
func normaliseEntry(rawName, prefix string) (string, bool) {
	if rawName == "" || strings.HasPrefix(rawName, "/") {
		return "", false
	}
	if strings.ContainsRune(rawName, 0) || strings.ContainsRune(rawName, '\\') {
		return "", false
	}
	cleaned := path.Clean(rawName)
	for _, part := range strings.Split(cleaned, "/") {
		if part == ".." || strings.HasPrefix(part, ".") {
			return "", false
		}
	}
	for i := 0; i < len(cleaned); i++ {
		if !isSafePathByte(cleaned[i]) {
			return "", false
		}
	}
	if prefix != "" {
		if cleaned != prefix && !strings.HasPrefix(cleaned, prefix+"/") {
			return "", false
		}
		cleaned = strings.TrimPrefix(strings.TrimPrefix(cleaned, prefix), "/")
		if cleaned == "" {
			return "", false
		}
	}
	return cleaned, true
}

func isSafePathByte(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	case b == '.' || b == '_' || b == '-' || b == '/':
		return true
	default:
		return false
	}
}

func (f *Fetcher) httpClient() *http.Client {
	if f.HTTPClient != nil {
		return f.HTTPClient
	}
	return http.DefaultClient
}

func (f *Fetcher) urlValidator() func(string) error { return f.URLValidator }

func (f *Fetcher) ipValidator() func(net.IP) error {
	if f.IPValidator != nil {
		return f.IPValidator
	}
	return forbiddenIP
}

func (f *Fetcher) maxArchiveBytes() int64 {
	return orDefault(f.MaxArchiveBytes, defaultMaxArchiveBytes)
}

func (f *Fetcher) maxPerEntryBytes() int64 {
	return orDefault(f.MaxPerEntryBytes, defaultMaxPerEntryBytes)
}

func (f *Fetcher) maxDecompressedBytes() int64 {
	return orDefault(f.MaxDecompressedBytes, defaultMaxDecompressedBytes)
}

func (f *Fetcher) maxExtractedBytes() int64 {
	return orDefault(f.MaxExtractedBytes, defaultMaxExtractedBytes)
}

func orDefault(v, def int64) int64 {
	if v > 0 {
		return v
	}
	return def
}

// safeDialContext resolves the host once, rejects the connection if any
// resolved IP is forbidden, then dials a validated address — closing the
// DNS-rebinding window between check and connect.
func (f *Fetcher) safeDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	check := f.ipValidator()
	for _, ipa := range ips {
		if err := check(ipa.IP); err != nil {
			return nil, fmt.Errorf("%w: %s", ErrForbiddenAddress, ipa.IP)
		}
	}
	var d net.Dialer
	var lastErr error
	for _, ipa := range ips {
		conn, derr := d.DialContext(ctx, network, net.JoinHostPort(ipa.IP.String(), port))
		if derr == nil {
			return conn, nil
		}
		lastErr = derr
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no addresses for %s", host)
	}
	return nil, lastErr
}

// verifyDigest checks the computed lowercase hex against an "<algo>:<hex>"
// expectation. Only sha256 is supported (source-controller's default).
func verifyDigest(gotHex, expected string) error {
	d := strings.ToLower(strings.TrimSpace(expected))
	colon := strings.IndexByte(d, ':')
	if colon <= 0 || colon == len(d)-1 {
		return fmt.Errorf("%w: %q", ErrDigestInvalid, expected)
	}
	algo, want := d[:colon], d[colon+1:]
	if algo != "sha256" {
		return fmt.Errorf("%w: unsupported algorithm %q", ErrDigestInvalid, algo)
	}
	if len(want) != sha256.Size*2 || !isHex(want) {
		return fmt.Errorf("%w: malformed sha256 %q", ErrDigestInvalid, want)
	}
	if gotHex != want {
		return fmt.Errorf("%w: declared sha256:%s, got sha256:%s", ErrDigestMismatch, want, gotHex)
	}
	return nil
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}
