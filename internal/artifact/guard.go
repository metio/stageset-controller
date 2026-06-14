// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package artifact

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
)

// URL/IP guard sentinels.
var (
	ErrInvalidScheme = errors.New("artifact url scheme must be http or https")
	ErrMissingHost   = errors.New("artifact url must include a host")
	ErrForbiddenHost = errors.New("artifact url host targets a forbidden surface")
)

// validateHTTPURL is the production URL guard: http(s) scheme, a host, and no
// literal loopback/link-local/multicast/unspecified address or "localhost".
//
// It deliberately does NOT block RFC1918 / CGNAT / IPv6-ULA private ranges —
// the artifact's producer is an in-cluster source server on private
// addresses, so blocking them would break the main use case. Reachability of
// internal services is NetworkPolicy's boundary.
func validateHTTPURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrForbiddenHost, err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return fmt.Errorf("%w: got %q", ErrInvalidScheme, u.Scheme)
	}
	host := strings.TrimSuffix(u.Hostname(), ".")
	if host == "" {
		return ErrMissingHost
	}
	if strings.EqualFold(host, "localhost") {
		return fmt.Errorf("%w: %s", ErrForbiddenHost, host)
	}
	if ip := net.ParseIP(host); ip != nil && forbiddenIP(ip) != nil {
		return fmt.Errorf("%w: %s", ErrForbiddenHost, host)
	}
	return nil
}

// forbiddenIP rejects loopback, link-local (incl. cloud metadata), multicast,
// and the unspecified address. Returns nil for a permitted IP.
func forbiddenIP(ip net.IP) error {
	switch {
	case ip.IsLoopback(),
		ip.IsLinkLocalUnicast(),
		ip.IsLinkLocalMulticast(),
		ip.IsMulticast(),
		ip.IsUnspecified():
		return fmt.Errorf("%w: %s", ErrForbiddenHost, ip)
	}
	return nil
}

// PermissiveHTTPURL validates only the scheme, skipping the host denylist. It
// is for tests and dev clusters reaching httptest listeners on loopback.
func PermissiveHTTPURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrForbiddenHost, err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		return nil
	}
	return fmt.Errorf("%w: got %q", ErrInvalidScheme, u.Scheme)
}

// PermissiveIP allows any address; for tests reaching loopback listeners.
func PermissiveIP(net.IP) error { return nil }

// errDecompressedCapped is the internal signal that the gzip stream exceeded
// the decompressed-byte cap; extract() maps it to ErrDecompressedTooLarge.
var errDecompressedCapped = errors.New("decompressed stream cap exceeded")

// cappedReader bounds the number of bytes read from the decompressed gzip
// stream, defeating gzip bombs whose inflated size dwarfs the compressed body.
type cappedReader struct {
	r         io.Reader
	remaining int64
}

func (c *cappedReader) Read(p []byte) (int, error) {
	if c.remaining <= 0 {
		return 0, errDecompressedCapped
	}
	if int64(len(p)) > c.remaining {
		p = p[:c.remaining]
	}
	n, err := c.r.Read(p)
	c.remaining -= int64(n)
	return n, err
}
