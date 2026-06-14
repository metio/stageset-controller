// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// StageInventorySpec holds one shard of a stage's inventory. Entries live
// in spec (not status) deliberately: backup tooling restores spec, so prune
// history survives disaster recovery. Shard zero additionally acts as the
// ApplySet (KEP-3659) parent object for the stage.
type StageInventorySpec struct {
	// StagePosition records the stage's position in spec.stages at write
	// time, used for reverse-order teardown of removed stages.
	// +optional
	StagePosition int32 `json:"stagePosition,omitempty"`

	// Entries lists the objects this shard accounts for.
	// +optional
	Entries []InventoryEntry `json:"entries,omitempty"`
}

// InventoryEntry identifies one tracked object.
type InventoryEntry struct {
	// ID in the form namespace_name_group_kind.
	// +required
	ID string `json:"id"`

	// V is the API version the object was last applied with.
	// +required
	V string `json:"v"`
}

// +kubebuilder:object:root=true
// +kubebuilder:printcolumn:name="Stage",type=string,JSONPath=`.metadata.labels.stages\.metio\.wtf/stage`
// +kubebuilder:printcolumn:name="Entries",type=integer,JSONPath=`.spec.entries[*]`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// StageInventory is one shard of a stage's recorded inventory.
type StageInventory struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec StageInventorySpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// StageInventoryList contains a list of StageInventory.
type StageInventoryList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []StageInventory `json:"items"`
}

// Well-known labels on StageInventory objects.
const (
	// StageSetLabel holds the owning StageSet's name.
	StageSetLabel = "stages.metio.wtf/stage-set"
	// StageLabel holds the stage name.
	StageLabel = "stages.metio.wtf/stage"
	// ShardLabel holds the shard index.
	ShardLabel = "stages.metio.wtf/shard"
	// PruneAnnotation opts a single object out of pruning when set to
	// "disabled".
	PruneAnnotation = "stages.metio.wtf/prune"
)
