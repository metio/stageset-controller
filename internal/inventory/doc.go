// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

// Package inventory implements the pure, dependency-free core of StageSet
// inventory handling: stable object identifiers, per-stage prune planning
// with cross-stage ownership transfer and reverse-order teardown of removed
// stages, deterministic shard packing for StageInventory objects, and the
// ApplySet (KEP-3659) metadata derivations.
//
// The package deliberately imports nothing outside the standard library so
// that its logic is testable without a cluster or any Kubernetes
// dependencies; this property is enforced by arch-go.
package inventory
