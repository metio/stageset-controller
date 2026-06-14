// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package stageinv

import (
	"testing"

	"github.com/metio/stageset-controller/internal/inventory"
)

func ref(name string) inventory.ObjectRef {
	return inventory.ObjectRef{Kind: "ConfigMap", Namespace: "ns", Name: name, Version: "v1"}
}

func TestDiff(t *testing.T) {
	t.Parallel()
	stored := []inventory.ObjectRef{ref("a"), ref("b"), ref("c")}
	current := []inventory.ObjectRef{ref("a"), ref("c")}
	pruned := Diff(stored, current)
	if len(pruned) != 1 || pruned[0].Name != "b" {
		t.Fatalf("Diff = %#v, want only [b]", pruned)
	}
}

func TestDiff_NothingRemoved(t *testing.T) {
	t.Parallel()
	refs := []inventory.ObjectRef{ref("a"), ref("b")}
	if got := Diff(refs, refs); len(got) != 0 {
		t.Fatalf("Diff of identical sets = %#v, want empty", got)
	}
}

func TestObjects(t *testing.T) {
	t.Parallel()
	objs := Objects([]inventory.ObjectRef{ref("x")})
	if len(objs) != 1 {
		t.Fatalf("want 1 object, got %d", len(objs))
	}
	if objs[0].GetName() != "x" || objs[0].GetKind() != "ConfigMap" || objs[0].GetNamespace() != "ns" {
		t.Fatalf("unexpected object: %#v", objs[0].Object)
	}
}
