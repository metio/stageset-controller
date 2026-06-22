// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package artifact

import (
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/url"
	"strconv"
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
	if ip := parseIPAny(host); ip != nil && forbiddenIP(ip) != nil {
		return fmt.Errorf("%w: %s", ErrForbiddenHost, host)
	}
	return nil
}

// parseIPAny parses a host as an IP, accepting both the canonical dotted-quad /
// IPv6 forms net.ParseIP handles AND the inet_aton-style alternate IPv4 forms a
// libc resolver honors but net.ParseIP rejects: a single integer
// (0x7f000001 / 2130706433 / 017700000001), or fewer than four dotted parts.
// Returns nil when the host is not an IP literal in any of these forms. This is
// the string-level layer's parity with the dial-time pin, which already resolves
// such a literal and rejects the loopback/link-local address it yields.
func parseIPAny(host string) net.IP {
	if ip := net.ParseIP(host); ip != nil {
		return ip
	}
	return parseInetAtonIPv4(host)
}

// ParseIPAny parses a host as a literal IP, including the inet_aton alternate
// forms (single-int, hex/octal, short-dotted) that net.ParseIP rejects but libc
// resolvers honor. Exported so the sibling SSRF guard in internal/actions shares
// one implementation instead of a weaker copy. Returns nil when host is not an
// IP literal in any recognized form.
func ParseIPAny(host string) net.IP { return parseIPAny(host) }

// parseInetAtonIPv4 decodes the historical inet_aton alternate IPv4 renderings.
// Each dotted part (1–4 of them) is parsed in C numeric base (0x… hex, 0…
// octal, else decimal); the final part absorbs all remaining low-order bytes, so
// "127.1" is 127.0.0.1 and "0x7f000001" is the whole 32-bit address. Returns nil
// on any out-of-range part or non-numeric input.
func parseInetAtonIPv4(host string) net.IP {
	if host == "" {
		return nil
	}
	parts := strings.Split(host, ".")
	if len(parts) > 4 {
		return nil
	}
	vals := make([]uint64, len(parts))
	for i, p := range parts {
		v, err := parseCNumber(p)
		if err != nil {
			return nil
		}
		vals[i] = v
	}
	// All leading parts are single bytes; the final part fills the remaining
	// 4-len(parts)+1 low-order bytes.
	var addr uint64
	for i := 0; i < len(vals)-1; i++ {
		if vals[i] > 0xff {
			return nil
		}
		addr |= vals[i] << (8 * (3 - uint(i)))
	}
	last := vals[len(vals)-1]
	maxLast := big.NewInt(1).Lsh(big.NewInt(1), uint(8*(4-len(vals)+1))).Uint64() - 1
	if last > maxLast {
		return nil
	}
	addr |= last
	if addr > 0xffffffff {
		return nil
	}
	// #nosec G115 -- addr is bounded to 32 bits above; each byte() truncation
	// deliberately extracts one octet of the validated address.
	return net.IPv4(byte(addr>>24), byte(addr>>16), byte(addr>>8), byte(addr)).To4()
}

// parseCNumber parses one inet_aton component in C numeric convention: a 0x/0X
// prefix is hex, a leading 0 (with more digits) is octal, otherwise decimal.
func parseCNumber(s string) (uint64, error) {
	if s == "" {
		return 0, strconv.ErrSyntax
	}
	switch {
	case len(s) > 2 && (s[0:2] == "0x" || s[0:2] == "0X"):
		return strconv.ParseUint(s[2:], 16, 64)
	case len(s) > 1 && s[0] == '0':
		return strconv.ParseUint(s[1:], 8, 64)
	default:
		return strconv.ParseUint(s, 10, 64)
	}
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
		// Budget spent. A stream sized exactly at the cap is fine — surface its
		// EOF; only a further byte means the cap is genuinely exceeded.
		var probe [1]byte
		n, err := c.r.Read(probe[:])
		if n > 0 {
			return 0, errDecompressedCapped
		}
		if err == nil {
			err = io.EOF
		}
		return 0, err
	}
	if int64(len(p)) > c.remaining {
		p = p[:c.remaining]
	}
	n, err := c.r.Read(p)
	c.remaining -= int64(n)
	return n, err
}
