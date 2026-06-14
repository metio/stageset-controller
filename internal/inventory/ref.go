// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package inventory

import (
	"fmt"
	"strings"
)

// ObjectRef identifies a single Kubernetes object tracked by a stage
// inventory. Identity is (Group, Kind, Namespace, Name); Version records the
// API version the object was last applied with and is not part of identity.
type ObjectRef struct {
	Group     string
	Kind      string
	Namespace string
	Name      string
	Version   string
}

const idSeparator = "_"

// ID returns the stable identifier of the object in the form
// "namespace_name_group_kind" ("_name_group_kind" for cluster-scoped
// objects, "namespace_name__kind" for core-group objects). The encoding is
// the one used by kustomize-controller and cli-utils. It is unambiguous
// because Kubernetes object names, namespaces, API groups, and kinds can
// never contain an underscore.
func (r ObjectRef) ID() string {
	return strings.Join([]string{r.Namespace, r.Name, r.Group, r.Kind}, idSeparator)
}

// ParseID is the inverse of ObjectRef.ID. The version is supplied separately
// because it is stored next to, not inside, the identifier.
func ParseID(id, version string) (ObjectRef, error) {
	parts := strings.Split(id, idSeparator)
	if len(parts) != 4 {
		return ObjectRef{}, fmt.Errorf("inventory: malformed id %q: expected 4 underscore-separated parts, got %d", id, len(parts))
	}
	ref := ObjectRef{
		Namespace: parts[0],
		Name:      parts[1],
		Group:     parts[2],
		Kind:      parts[3],
		Version:   version,
	}
	if ref.Name == "" {
		return ObjectRef{}, fmt.Errorf("inventory: malformed id %q: empty name", id)
	}
	if ref.Kind == "" {
		return ObjectRef{}, fmt.Errorf("inventory: malformed id %q: empty kind", id)
	}
	return ref, nil
}

// ClusterScoped reports whether the reference denotes a cluster-scoped
// object.
func (r ObjectRef) ClusterScoped() bool {
	return r.Namespace == ""
}
