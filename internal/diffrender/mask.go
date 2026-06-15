// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

// Package diffrender renders Kubernetes objects for human inspection: Secret
// masking, server-noise stripping, multi-document YAML, and (for diff) unified
// per-object diffs with a change summary. It depends only on unstructured
// objects and a YAML encoder — no cluster — so the masking and stripping rules
// are pinned by unit tests.
package diffrender

import (
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// maskPlaceholder formats the masked stand-in for a Secret value. The index
// correlates identical values across every object in one render, so an operator
// sees that a value changed (and whether two keys share a value) without the
// plaintext ever reaching the terminal or a CI log.
func maskPlaceholder(index int) string {
	return fmt.Sprintf("<-- value not shown (#%d)", index)
}

// SecretMasker replaces Secret data/stringData values with stable placeholders.
// A single masker is shared across all objects (and both sides of a diff) in
// one render so equal values map to equal placeholders — equal values never
// show as a spurious change.
type SecretMasker struct {
	reveal bool
	seen   map[string]int
	next   int
}

// NewSecretMasker returns a masker. When reveal is true it is a no-op, so the
// same render path serves both --show-secrets and the masked default.
func NewSecretMasker(reveal bool) *SecretMasker {
	return &SecretMasker{reveal: reveal, seen: map[string]int{}}
}

// Mask rewrites the Secret's data and stringData values in place. Non-Secret
// objects and a reveal-mode masker are left untouched.
func (m *SecretMasker) Mask(obj *unstructured.Unstructured) {
	if m == nil || m.reveal || obj == nil || !isSecret(obj) {
		return
	}
	for _, field := range []string{"data", "stringData"} {
		vals, found, err := unstructured.NestedMap(obj.Object, field)
		if err != nil || !found {
			continue
		}
		for k, v := range vals {
			vals[k] = maskPlaceholder(m.indexFor(fmt.Sprintf("%v", v)))
		}
		_ = unstructured.SetNestedMap(obj.Object, vals, field)
	}
}

// indexFor returns a stable 1-based index for a raw value, assigning a new one
// the first time the value is seen.
func (m *SecretMasker) indexFor(raw string) int {
	if idx, ok := m.seen[raw]; ok {
		return idx
	}
	m.next++
	m.seen[raw] = m.next
	return m.next
}

// isSecret reports whether obj is a core/v1 Secret.
func isSecret(obj *unstructured.Unstructured) bool {
	if obj.GetKind() != "Secret" {
		return false
	}
	switch obj.GetAPIVersion() {
	case "v1", "":
		return true
	default:
		return false
	}
}
