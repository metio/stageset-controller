// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package inventory

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func refs(n int) []ObjectRef {
	out := make([]ObjectRef, 0, n)
	for i := range n {
		out = append(out, ObjectRef{Kind: "ConfigMap", Namespace: "ns", Name: fmt.Sprintf("cm-%04d", i), Version: "v1"})
	}
	return out
}

func TestPlanShardsRejectsNonPositiveCapacity(t *testing.T) {
	t.Parallel()
	for _, capacity := range []int{0, -1} {
		if _, err := PlanShards(refs(1), capacity); err == nil {
			t.Errorf("capacity %d: expected error", capacity)
		}
	}
}

func TestPlanShardsEmptyYieldsSingleEmptyShard(t *testing.T) {
	t.Parallel()
	shards, err := PlanShards(nil, DefaultShardCap)
	if err != nil {
		t.Fatal(err)
	}
	if len(shards) != 1 || len(shards[0]) != 0 {
		t.Fatalf("expected one empty shard (the ApplySet parent), got %+v", shards)
	}
}

func TestPlanShardsPacking(t *testing.T) {
	t.Parallel()
	tests := []struct {
		entries  int
		capacity int
		want     []int // entries per shard
	}{
		{entries: 1, capacity: 5, want: []int{1}},
		{entries: 5, capacity: 5, want: []int{5}},
		{entries: 6, capacity: 5, want: []int{5, 1}},
		{entries: 10, capacity: 5, want: []int{5, 5}},
		{entries: 11, capacity: 5, want: []int{5, 5, 1}},
	}
	for _, tc := range tests {
		shards, err := PlanShards(refs(tc.entries), tc.capacity)
		if err != nil {
			t.Fatal(err)
		}
		got := make([]int, 0, len(shards))
		for _, shard := range shards {
			got = append(got, len(shard))
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("entries=%d capacity=%d: shard sizes %v, want %v", tc.entries, tc.capacity, got, tc.want)
		}
	}
}

func TestPlanShardsDeterministicRegardlessOfInputOrder(t *testing.T) {
	t.Parallel()
	forward := refs(7)
	backward := make([]ObjectRef, len(forward))
	for i, ref := range forward {
		backward[len(forward)-1-i] = ref
	}

	a, err := PlanShards(forward, 3)
	if err != nil {
		t.Fatal(err)
	}
	b, err := PlanShards(backward, 3)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("shard plan depends on input order:\n%+v\nvs\n%+v", a, b)
	}
}

func TestPlanShardsDoesNotMutateInput(t *testing.T) {
	t.Parallel()
	input := []ObjectRef{
		{Kind: "ConfigMap", Namespace: "ns", Name: "zz"},
		{Kind: "ConfigMap", Namespace: "ns", Name: "aa"},
	}
	original := reflect.ValueOf(input).Interface()
	if _, err := PlanShards(input, 1); err != nil {
		t.Fatal(err)
	}
	if input[0].Name != "zz" {
		t.Fatalf("input mutated: %+v (original %+v)", input, original)
	}
}

func TestShardName(t *testing.T) {
	t.Parallel()
	if got := ShardName("platform", "operators", 0); got != "platform-operators-00" {
		t.Fatalf("ShardName = %q", got)
	}
	if got := ShardName("platform", "operators", 12); got != "platform-operators-12" {
		t.Fatalf("ShardName = %q", got)
	}
}

func TestShardNameTruncatesOverlongNames(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a", 200)
	other := strings.Repeat("a", 200) + "b"

	nameA := ShardName(long, long, 1)
	nameB := ShardName(other, other, 1)

	if len(nameA) > 253 {
		t.Fatalf("name exceeds DNS-1123 limit: %d chars", len(nameA))
	}
	if nameA == nameB {
		t.Fatal("distinct long inputs must not collide after truncation")
	}
}
