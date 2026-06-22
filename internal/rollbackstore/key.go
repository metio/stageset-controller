// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package rollbackstore

import (
	"fmt"
	"io"
	"strings"
)

// MaxObjectBytes bounds a single rollback object read so a planted oversized
// object (a shared RWX PV or bucket the controller writes to is also writable by
// whoever has volume/bucket access) can't OOM the lease-holder when it
// JSON-decodes a snapshot during rollback. Rendered per-stage manifests are far
// below this in normal use.
const MaxObjectBytes int64 = 64 << 20

// readCapped reads r into memory, failing if the content exceeds MaxObjectBytes.
// The +1 read distinguishes "exactly at the cap" (allowed) from "over it".
func readCapped(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxObjectBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > MaxObjectBytes {
		return nil, fmt.Errorf("rollback object exceeds %d bytes", MaxObjectBytes)
	}
	return data, nil
}

// Key addresses a stage's rendered output in the store:
// "<namespace>/<name>/<stage>/<digest>", with ':' replaced by '-' so the
// algo:hex digest is safe as a path segment and an object name. The controller
// (writer) and the MCP diff_revisions tool (reader) both derive the key here, so
// the addressing stays a single contract neither side can drift from.
func Key(namespace, name, stage, digest string) string {
	return fmt.Sprintf("%s/%s/%s/%s", namespace, name, stage, strings.ReplaceAll(digest, ":", "-"))
}

// ValidDigest reports whether digest has the canonical "<algo>:<hex>" artifact
// shape. The reconcile (writer) path always passes a digest already verified by
// the fetcher, but a reader such as the MCP diff_revisions tool takes the digest
// from the caller — and on the S3 backend the digest becomes part of the object
// name, so a '/'- or '..'-bearing value would address a different object within
// the bucket prefix. Rejecting anything that isn't algo:hex keeps the key a safe
// single segment on every backend.
func ValidDigest(digest string) error {
	algo, hex, ok := strings.Cut(digest, ":")
	if !ok || algo == "" || hex == "" {
		return fmt.Errorf("digest %q must be of the form <algo>:<hex>", digest)
	}
	for _, r := range algo {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
			return fmt.Errorf("digest %q has an invalid algorithm", digest)
		}
	}
	for _, r := range hex {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
			return fmt.Errorf("digest %q has a non-hex value", digest)
		}
	}
	return nil
}
