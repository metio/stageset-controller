// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// StageLedgerSpec is the user-authored half of a StageLedger: standing
// assertions that an action already completed, committable to Git alongside the
// StageSet. The controller promotes each resolvable entry into
// status.completedActions with origin Baselined before evaluating any Lifetime
// action.
type StageLedgerSpec struct {
	// Baseline lists actions to treat as already completed without running them
	// — adoption of a system whose bootstrap ran elsewhere. It is additive-only:
	// removing an entry never revokes a recorded completion (spec understating
	// status is the tolerable direction, and is visible in status). Forgetting a
	// completion means editing status.completedActions directly.
	// +optional
	Baseline []LedgerRef `json:"baseline,omitempty"`
}

// LedgerRef names one stage's action.
type LedgerRef struct {
	// Stage is the stage the action belongs to.
	// +required
	Stage string `json:"stage"`
	// Action is the action's name within that stage.
	// +required
	Action string `json:"action"`
}

// LedgerConditionBaselineValid is the StageLedger condition reporting whether
// every spec.baseline entry resolves to a real scope: Lifetime action in the
// same-named StageSet. Status False (reason Unresolved) means one or more
// entries are held — not promoted — until a spec change resolves them. It is a
// wire-stable condition type; downstream tooling may match on it.
const LedgerConditionBaselineValid = "BaselineValid"

// LedgerOrigin distinguishes an action the controller ran from one an operator
// asserted was already done.
type LedgerOrigin string

const (
	// OriginExecuted marks a completion the controller itself performed.
	OriginExecuted LedgerOrigin = "Executed"
	// OriginBaselined marks a completion asserted via spec.baseline (adoption):
	// the effect is presumed already present; the action was not run.
	OriginBaselined LedgerOrigin = "Baselined"
)

// StageLedgerStatus is the controller-owned record of once-per-lifetime action
// completions. It is written under the controller's own identity and is never
// rebuilt from cluster state — that is the whole reason it lives in a dedicated
// object rather than in a StageSet's status or its StageInventory, which the
// inventory self-heal reconstructs from live objects.
type StageLedgerStatus struct {
	// CompletedActions records every scope: Lifetime action that has completed
	// (or been baselined) for this StageSet, so it never runs again.
	// +optional
	CompletedActions []LedgerCompletion `json:"completedActions,omitempty"`
	// Conditions carries the ledger's own conditions. The BaselineValid condition
	// reports whether every spec.baseline entry resolves to a real scope: Lifetime
	// action in the same-named StageSet; an unresolvable entry is held (never
	// promoted) and surfaced here rather than silently dropped, so a typo does not
	// masquerade as a recorded completion.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +patchStrategy=merge
	// +patchMergeKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// LedgerCompletion records one lifetime-scoped action's completion. Identity is
// (stage, action); the digest is audit metadata only — republishing a job
// artifact must not resurrect a completed bootstrap.
type LedgerCompletion struct {
	// Stage the action belonged to.
	// +required
	Stage string `json:"stage"`
	// Action name.
	// +required
	Action string `json:"action"`
	// Origin is Executed (the controller ran it) or Baselined (asserted via
	// spec.baseline without running).
	// +kubebuilder:validation:Enum=Executed;Baselined
	// +required
	Origin LedgerOrigin `json:"origin"`
	// Digest is the action's resolved content at completion, recorded for audit.
	// It is not part of identity.
	// +optional
	Digest string `json:"digest,omitempty"`
	// Anchor, present when the action declared a completionAnchor, records the
	// witness object's identity — including its UID at completion time. The
	// completion is valid only while that object still exists with this UID; a
	// changed UID (a delete+recreate under the same name) invalidates it and the
	// action runs again. Absent for an unanchored (external-effect) completion,
	// which is retained unconditionally.
	// +optional
	Anchor *AnchorWitness `json:"anchor,omitempty"`
	// CompletedAt is when the completion was recorded.
	// +required
	CompletedAt metav1.Time `json:"completedAt"`
}

// AnchorWitness records the identity of a completionAnchor object at the moment
// an anchored scope: Lifetime action completed. The UID is the load-bearing
// field: existence under the same name is insufficient, because a same-named
// recreation is a fresh, empty object.
type AnchorWitness struct {
	// APIVersion of the witness object.
	// +required
	APIVersion string `json:"apiVersion"`
	// Kind of the witness object.
	// +required
	Kind string `json:"kind"`
	// Name of the witness object.
	// +required
	Name string `json:"name"`
	// UID recorded at completion. A different UID under the same name invalidates
	// the completion.
	// +required
	UID string `json:"uid"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=sledger
// +kubebuilder:printcolumn:name="Completed",type=integer,JSONPath=`.status.completedActions[*].action`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// StageLedger is the durable, once-per-lifetime action ledger for the
// same-named StageSet. It is deliberately NOT owner-referenced to the StageSet:
// it survives the StageSet's deletion and is adopted by name on recreate, so a
// bootstrap recorded here is not forgotten by a delete-and-reapply. It is the
// durable kernel of a Helm release record without the rest of Helm's state.
type StageLedger struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   StageLedgerSpec   `json:"spec,omitempty"`
	Status StageLedgerStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// StageLedgerList contains a list of StageLedger.
type StageLedgerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []StageLedger `json:"items"`
}

// IsCompleted reports whether (stage, action) is already recorded complete —
// the gate for a scope: Lifetime action.
func (s *StageLedger) IsCompleted(stage, action string) bool {
	for i := range s.Status.CompletedActions {
		c := &s.Status.CompletedActions[i]
		if c.Stage == stage && c.Action == action {
			return true
		}
	}
	return false
}
