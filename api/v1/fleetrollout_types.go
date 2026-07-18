// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// FleetRolloutSpec declares a progressive rollout of one target version across a
// selected set of StageSets, in ordered waves.
// +kubebuilder:validation:XValidation:rule="self.onRegression != 'Rollback' || (has(self.previousVersion) && size(self.previousVersion) > 0)",message="previousVersion is required when onRegression is Rollback"
type FleetRolloutSpec struct {
	// TargetVersion is the version this rollout approves across the fleet. The
	// controller stamps it as each wave opens; a member advances to it only when
	// its own source already offers that version, so the rollout paces adoption
	// rather than pushing versions — it composes with GitOps.
	// +required
	TargetVersion string `json:"targetVersion"`

	// Selector chooses the StageSets that participate in this rollout. A member
	// must also set spec.version.approvalMode: Always so it holds each advance for
	// the fleet to approve.
	// +required
	Selector metav1.LabelSelector `json:"selector"`

	// NamespaceSelector bounds which namespaces the fleet spans. Omit to consider
	// StageSets in every namespace.
	// +optional
	NamespaceSelector *metav1.LabelSelector `json:"namespaceSelector,omitempty"`

	// Waves are the ordered groups the rollout opens one at a time. A StageSet
	// that matches Selector but no wave is reported and never approved — no silent
	// exclusion.
	// +required
	// +kubebuilder:validation:MinItems=1
	Waves []FleetWave `json:"waves"`

	// OnRegression is what to do when a wave's health gate fails or a member
	// regresses: Halt (default) stops approving further waves; Rollback also
	// directs the affected wave's members back to PreviousVersion, unwinding them
	// via each member's down migrations where they exist.
	// +kubebuilder:validation:Enum=Halt;Rollback
	// +kubebuilder:default=Halt
	// +optional
	OnRegression string `json:"onRegression,omitempty"`

	// PreviousVersion is the version a Rollback directs regressed members back to —
	// the version the fleet is rolling away from. Required when onRegression is
	// Rollback. A member only actually reverts if it permits downgrades
	// (spec.version.allowDowngrade) and the crossed migrations declare down actions;
	// otherwise it refuses, and the fleet stays halted.
	// +optional
	PreviousVersion string `json:"previousVersion,omitempty"`
}

// FleetWave is one ordered group of the rollout.
type FleetWave struct {
	// Name identifies the wave in status and events.
	// +required
	Name string `json:"name"`

	// Selector partitions the fleet: members matching it belong to this wave.
	// Waves should not overlap; a member in two waves is reported as ambiguous.
	// +required
	Selector metav1.LabelSelector `json:"selector"`

	// Soak holds the wave after it settles (every member at the target version and
	// Ready) before the health gate runs and the next wave opens.
	// +optional
	Soak *metav1.Duration `json:"soak,omitempty"`

	// Gate is a metric the wave must satisfy after it settles and soaks before the
	// next wave opens. A scalar outside the threshold halts the whole rollout.
	// +optional
	Gate *FleetWaveGate `json:"gate,omitempty"`
}

// FleetWaveGate is a metric health check evaluated once a wave has settled and
// soaked. The query runs under the controller's own identity; scope it to the
// wave in the query itself (e.g. a PromQL selector).
type FleetWaveGate struct {
	// Source is the metric to query — a Prometheus instant query or a webhook.
	// +required
	Source MetricSource `json:"source"`

	// Threshold the queried scalar must satisfy for the wave to pass. A scalar
	// outside it halts the rollout.
	// +required
	Threshold Threshold `json:"threshold"`
}

// FleetPhase is the overall state of a rollout.
type FleetPhase string

const (
	// FleetPending: the rollout has not opened its first wave yet.
	FleetPending FleetPhase = "Pending"
	// FleetInProgress: a wave is open and advancing.
	FleetInProgress FleetPhase = "InProgress"
	// FleetHalted: a wave regressed or failed its gate; no further waves open.
	FleetHalted FleetPhase = "Halted"
	// FleetCompleted: every wave reached the target version and passed its gate.
	FleetCompleted FleetPhase = "Completed"
)

// FleetRolloutStatus reports rollout progress.
type FleetRolloutStatus struct {
	// ObservedGeneration is the spec generation this status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Phase is the overall rollout state.
	// +optional
	Phase FleetPhase `json:"phase,omitempty"`

	// CurrentWave is the wave currently open, empty before the first opens or
	// after completion.
	// +optional
	CurrentWave string `json:"currentWave,omitempty"`

	// Waves reports per-wave progress in spec order.
	// +optional
	// +listType=map
	// +listMapKey=name
	Waves []FleetWaveStatus `json:"waves,omitempty"`

	// Conditions carries the Ready condition (with ObservedGeneration) for the
	// rollout as a whole.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// FleetWaveStatus is one wave's observed progress.
type FleetWaveStatus struct {
	// Name of the wave.
	// +required
	Name string `json:"name"`
	// Total members assigned to this wave.
	// +optional
	Total int32 `json:"total,omitempty"`
	// AtTarget counts members whose status.version equals the target version.
	// +optional
	AtTarget int32 `json:"atTarget,omitempty"`
	// Ready counts members that are Ready at their observed generation with no
	// revision still held back.
	// +optional
	Ready int32 `json:"ready,omitempty"`
	// Settled is true once every member is both at the target version and Ready.
	// +optional
	Settled bool `json:"settled,omitempty"`
	// SoakUntil is when the wave's soak elapses, set once it settles.
	// +optional
	SoakUntil *metav1.Time `json:"soakUntil,omitempty"`
	// Health is the last verdict of the wave's health gate.
	// +kubebuilder:validation:Enum=Passing;Failing;Unknown
	// +optional
	Health string `json:"health,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=fleet
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.targetVersion`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Wave",type=string,JSONPath=`.status.currentWave`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// FleetRollout progressively rolls one target version across a fleet of StageSets
// in ordered waves, soaking and health-checking between waves and halting the
// whole fleet on regression. It is cluster-scoped because a fleet inherently spans
// namespaces; it approves the version its members' own sources already offer,
// pacing adoption rather than pushing versions.
type FleetRollout struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   FleetRolloutSpec   `json:"spec,omitempty"`
	Status FleetRolloutStatus `json:"status,omitempty"`
}

// GetConditions returns the rollout's status conditions, satisfying the fluxcd
// conditions interface so the shared patch helper and conditions.Set can be used.
func (in *FleetRollout) GetConditions() []metav1.Condition {
	return in.Status.Conditions
}

// SetConditions sets the rollout's status conditions.
func (in *FleetRollout) SetConditions(conditions []metav1.Condition) {
	in.Status.Conditions = conditions
}

// +kubebuilder:object:root=true

// FleetRolloutList is a list of FleetRollout.
type FleetRolloutList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []FleetRollout `json:"items"`
}
