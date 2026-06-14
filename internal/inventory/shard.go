// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package inventory

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
)

// DefaultShardCap is the default maximum number of entries per
// StageInventory shard. It corresponds to the --inventory-shard-cap
// controller flag and keeps a fully loaded shard comfortably below the
// ~1.5 MiB etcd object ceiling.
const DefaultShardCap = 5000

// maxNameLength is the DNS-1123 subdomain limit for object names.
const maxNameLength = 253

// PlanShards deterministically packs entries into shards of at most
// capacity entries each. Entries are sorted by ID first, so the same set
// always produces the same shards regardless of input order. An empty entry
// set yields a single empty shard, because shard zero doubles as the
// ApplySet parent and must exist even for an empty stage.
func PlanShards(entries []ObjectRef, capacity int) ([][]ObjectRef, error) {
	if capacity < 1 {
		return nil, fmt.Errorf("inventory: shard capacity must be positive, got %d", capacity)
	}
	sorted := slices.Clone(entries)
	slices.SortFunc(sorted, func(a, b ObjectRef) int { return cmp.Compare(a.ID(), b.ID()) })
	if len(sorted) == 0 {
		return [][]ObjectRef{{}}, nil
	}
	shards := make([][]ObjectRef, 0, (len(sorted)+capacity-1)/capacity)
	for start := 0; start < len(sorted); start += capacity {
		end := min(start+capacity, len(sorted))
		shards = append(shards, sorted[start:end:end])
	}
	return shards, nil
}

// ShardName returns the object name of a shard:
// "<stageset>-<stage>-<NN>". Names exceeding the DNS-1123 subdomain limit
// are truncated and suffixed with a hash of the full logical name so that
// distinct long inputs can never collide after truncation.
func ShardName(stageSet, stage string, shard int) string {
	name := fmt.Sprintf("%s-%s-%02d", stageSet, stage, shard)
	if len(name) <= maxNameLength {
		return name
	}
	digest := sha256.Sum256([]byte(name))
	suffix := hex.EncodeToString(digest[:])[:10]
	truncated := strings.TrimRight(name[:maxNameLength-len(suffix)-1], "-")
	return truncated + "-" + suffix
}
