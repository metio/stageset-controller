// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package inventory

import (
	"crypto/sha256"
	"encoding/base64"
	"slices"
	"strings"
)

// ApplySet (KEP-3659) metadata keys. These are the upstream, versioned
// on-object specification keys; the controller writes them in the hybrid
// and applyset inventory modes.
const (
	// IDLabel marks an ApplySet parent object with its set identifier.
	IDLabel = "applyset.kubernetes.io/id"
	// PartOfLabel marks a member object with the identifier of the set it
	// belongs to.
	PartOfLabel = "applyset.kubernetes.io/part-of"
	// IsParentTypeLabel must be present on a CRD whose instances act as
	// ApplySet parents.
	IsParentTypeLabel = "applyset.kubernetes.io/is-parent-type"
	// ToolingAnnotation identifies the tool managing the set.
	ToolingAnnotation = "applyset.kubernetes.io/tooling"
	// ContainsGroupKindsAnnotation hints which group-kinds the set
	// contains, bounding discovery.
	ContainsGroupKindsAnnotation = "applyset.kubernetes.io/contains-group-kinds"
	// AdditionalNamespacesAnnotation hints which namespaces other than the
	// parent's contain members.
	AdditionalNamespacesAnnotation = "applyset.kubernetes.io/additional-namespaces"

	// Tooling is the value this controller writes into ToolingAnnotation.
	Tooling = "stageset-controller/v1"
)

// ApplySetID derives the V1 ApplySet identifier for a parent object:
//
//	applyset-<base64url(sha256(name.namespace.kind.group))>-v1
//
// using unpadded URL-safe base64, per the KEP-3659 specification.
func ApplySetID(name, namespace, kind, group string) string {
	sum := sha256.Sum256([]byte(name + "." + namespace + "." + kind + "." + group))
	return "applyset-" + base64.RawURLEncoding.EncodeToString(sum[:]) + "-v1"
}

// ContainsGroupKinds renders the contains-group-kinds hint annotation value
// for the given members: a sorted, deduplicated, comma-separated list of
// "Kind.group" tokens, where core-group kinds appear as bare "Kind".
func ContainsGroupKinds(entries []ObjectRef) string {
	seen := make(map[string]struct{}, len(entries))
	tokens := make([]string, 0, len(entries))
	for _, ref := range entries {
		token := ref.Kind
		if ref.Group != "" {
			token += "." + ref.Group
		}
		if _, dup := seen[token]; dup {
			continue
		}
		seen[token] = struct{}{}
		tokens = append(tokens, token)
	}
	slices.Sort(tokens)
	return strings.Join(tokens, ",")
}

// AdditionalNamespaces renders the additional-namespaces hint annotation
// value: the sorted, deduplicated namespaces of all namespaced members,
// excluding the parent's own namespace.
func AdditionalNamespaces(entries []ObjectRef, parentNamespace string) string {
	seen := make(map[string]struct{}, len(entries))
	namespaces := make([]string, 0, len(entries))
	for _, ref := range entries {
		if ref.Namespace == "" || ref.Namespace == parentNamespace {
			continue
		}
		if _, dup := seen[ref.Namespace]; dup {
			continue
		}
		seen[ref.Namespace] = struct{}{}
		namespaces = append(namespaces, ref.Namespace)
	}
	slices.Sort(namespaces)
	return strings.Join(namespaces, ",")
}
