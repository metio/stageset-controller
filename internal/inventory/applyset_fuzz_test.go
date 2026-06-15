// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package inventory

import (
	"testing"
)

// FuzzApplySetID pins the two properties downstream tooling depends on for any
// inputs: the ID always matches the KEP-3659 V1 format (so `kubectl get -l
// applyset.kubernetes.io/part-of=<id>` is always a valid selector), and it is
// deterministic. Discrimination is deliberately NOT asserted: the inputs are
// concatenated with "." separators, so distinct field tuples can legitimately
// collide (e.g. name "a.b"+ns "c" vs name "a"+ns "b.c"); only sha256 guards
// genuine distinctness, which fuzzing cannot break.
func FuzzApplySetID(f *testing.F) {
	f.Add("parent", "ns", "StageInventory", "stages.metio.wtf")
	f.Add("", "", "", "")
	f.Add("a.b.c", "n", "k", "g")
	f.Add("name-with-😀-rune", "ns", "Kind", "group.example.io")
	f.Fuzz(func(t *testing.T, name, namespace, kind, group string) {
		id := ApplySetID(name, namespace, kind, group)
		if !idPattern.MatchString(id) {
			t.Fatalf("ApplySetID(%q,%q,%q,%q)=%q does not match V1 format", name, namespace, kind, group, id)
		}
		if again := ApplySetID(name, namespace, kind, group); again != id {
			t.Fatalf("ApplySetID not deterministic: %q vs %q", id, again)
		}
	})
}

// FuzzShardName asserts the shard object name never exceeds the DNS-1123
// subdomain limit and is deterministic, for any stageSet/stage/shard. Long
// inputs are truncated and hash-suffixed; the cap must hold regardless.
func FuzzShardName(f *testing.F) {
	f.Add("app", "first", 0)
	f.Add("", "", 0)
	f.Add("a", "b", -1)
	f.Add("a", "b", 9999)
	// A long input that forces the truncate-and-hash branch.
	f.Add(longString(300), "stage", 3)
	f.Fuzz(func(t *testing.T, stageSet, stage string, shard int) {
		name := ShardName(stageSet, stage, shard)
		if len(name) > maxNameLength {
			t.Fatalf("ShardName length %d exceeds the DNS-1123 limit %d: %q", len(name), maxNameLength, name)
		}
		if again := ShardName(stageSet, stage, shard); again != name {
			t.Fatalf("ShardName not deterministic: %q vs %q", name, again)
		}
	})
}

func longString(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'a'
	}
	return string(b)
}
