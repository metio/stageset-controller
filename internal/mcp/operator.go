/*
 * SPDX-FileCopyrightText: The stageset-controller Authors
 * SPDX-License-Identifier: 0BSD
 */

package mcp

import (
	"context"
	"fmt"
	"strings"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

// conditionReady is the Ready condition type the controller stamps. It matches
// controller.ConditionReady but is duplicated here so this package takes no
// dependency on the controller package.
const conditionReady = "Ready"

// stageSetSummary is the per-StageSet row returned by list_stagesets.
type stageSetSummary struct {
	Namespace          string `json:"namespace"`
	Name               string `json:"name"`
	Ready              string `json:"ready" jsonschema:"the Ready condition status: True, False, or Unknown"`
	Reason             string `json:"reason,omitempty" jsonschema:"the Ready condition reason (a wire-stable code)"`
	Suspended          bool   `json:"suspended"`
	Version            string `json:"version,omitempty" jsonschema:"the version currently rolled out, when version tracking is configured"`
	ObservedGeneration int64  `json:"observedGeneration"`
}

type stageView struct {
	Name            string `json:"name"`
	Phase           string `json:"phase,omitempty" jsonschema:"the stage phase: Pending, Applying, Pruning, Verifying, Ready, or Failed"`
	AppliedRevision string `json:"appliedRevision,omitempty"`
	Message         string `json:"message,omitempty"`
}

// stageSetDetail is the full per-StageSet view returned by get_stageset.
type stageSetDetail struct {
	Namespace            string            `json:"namespace"`
	Name                 string            `json:"name"`
	Ready                string            `json:"ready" jsonschema:"the Ready condition status: True, False, or Unknown"`
	Reason               string            `json:"reason,omitempty" jsonschema:"the Ready condition reason (a wire-stable code)"`
	Message              string            `json:"message,omitempty" jsonschema:"the Ready condition human-readable message"`
	RunbookURL           string            `json:"runbookURL,omitempty" jsonschema:"the per-reason remediation page for the current reason"`
	Suspended            bool              `json:"suspended"`
	ObservedGeneration   int64             `json:"observedGeneration"`
	Version              string            `json:"version,omitempty"`
	Stages               []stageView       `json:"stages,omitempty" jsonschema:"per-stage phase and applied revision, in spec order"`
	LastAppliedRevisions map[string]string `json:"lastAppliedRevisions,omitempty" jsonschema:"the source revision last applied per stage"`
	PendingMigrations    []string          `json:"pendingMigrations,omitempty" jsonschema:"migrations queued but not yet executed"`
}

type listStageSetsInput struct {
	Namespace string `json:"namespace,omitempty" jsonschema:"namespace to list; empty lists StageSets across all namespaces the controller can read"`
}

type listStageSetsOutput struct {
	StageSets []stageSetSummary `json:"stageSets"`
}

type getStageSetInput struct {
	Namespace string `json:"namespace" jsonschema:"the StageSet's namespace"`
	Name      string `json:"name" jsonschema:"the StageSet's name"`
}

func registerStageSetTools(server *mcpsdk.Server, cfg Config) {
	mcpsdk.AddTool(server, &mcpsdk.Tool{
		Name:        "list_stagesets",
		Description: "List StageSet resources with their Ready status, reason, suspend state, rolled-out version, and observed generation. Omit namespace to list across all namespaces the controller can read.",
	}, cfg.listStageSetsHandler)

	mcpsdk.AddTool(server, &mcpsdk.Tool{
		Name:        "get_stageset",
		Description: "Get one StageSet's full status: the Ready condition (status, reason, message), the per-reason runbook URL, suspend state, rolled-out version, per-stage phases and applied revisions, and any pending migrations.",
	}, cfg.getStageSetHandler)
}

func (cfg Config) listStageSetsHandler(ctx context.Context, _ *mcpsdk.CallToolRequest, in listStageSetsInput) (*mcpsdk.CallToolResult, listStageSetsOutput, error) {
	var list stagesv1.StageSetList
	var opts []client.ListOption
	if in.Namespace != "" {
		opts = append(opts, client.InNamespace(in.Namespace))
	}
	if err := cfg.KubeClient.List(ctx, &list, opts...); err != nil {
		return errorResult(fmt.Sprintf("cannot list StageSets: %v", err)), listStageSetsOutput{}, nil
	}
	out := listStageSetsOutput{StageSets: make([]stageSetSummary, 0, len(list.Items))}
	for i := range list.Items {
		s := &list.Items[i]
		ready, reason, _ := readyCondition(s)
		out.StageSets = append(out.StageSets, stageSetSummary{
			Namespace:          s.Namespace,
			Name:               s.Name,
			Ready:              ready,
			Reason:             reason,
			Suspended:          s.Spec.Suspend,
			Version:            s.Status.Version,
			ObservedGeneration: s.Status.ObservedGeneration,
		})
	}
	return nil, out, nil
}

func (cfg Config) getStageSetHandler(ctx context.Context, _ *mcpsdk.CallToolRequest, in getStageSetInput) (*mcpsdk.CallToolResult, stageSetDetail, error) {
	if in.Namespace == "" || in.Name == "" {
		return errorResult("both namespace and name are required"), stageSetDetail{}, nil
	}
	var ss stagesv1.StageSet
	if err := cfg.KubeClient.Get(ctx, client.ObjectKey{Namespace: in.Namespace, Name: in.Name}, &ss); err != nil {
		return errorResult(fmt.Sprintf("cannot get StageSet %s/%s: %v", in.Namespace, in.Name, err)), stageSetDetail{}, nil
	}

	ready, reason, message := readyCondition(&ss)
	detail := stageSetDetail{
		Namespace:            ss.Namespace,
		Name:                 ss.Name,
		Ready:                ready,
		Reason:               reason,
		Message:              message,
		RunbookURL:           cfg.runbookURL(reason),
		Suspended:            ss.Spec.Suspend,
		ObservedGeneration:   ss.Status.ObservedGeneration,
		Version:              ss.Status.Version,
		LastAppliedRevisions: ss.Status.LastAppliedRevisions,
		PendingMigrations:    ss.Status.PendingMigrations,
	}
	for _, st := range ss.Status.Stages {
		detail.Stages = append(detail.Stages, stageView{
			Name:            st.Name,
			Phase:           string(st.Phase),
			AppliedRevision: st.AppliedRevision,
			Message:         st.Message,
		})
	}
	return nil, detail, nil
}

// readyCondition extracts the Ready condition's status/reason/message. A
// StageSet with no Ready condition yet reports status "Unknown".
func readyCondition(ss *stagesv1.StageSet) (status, reason, message string) {
	cond := apimeta.FindStatusCondition(ss.Status.Conditions, conditionReady)
	if cond == nil {
		return string(metav1.ConditionUnknown), "", ""
	}
	return string(cond.Status), cond.Reason, cond.Message
}

// runbookURL builds the per-reason remediation page link, matching the
// controller's decorateMessage convention. Empty when there's no reason or no
// configured base URL.
func (cfg Config) runbookURL(reason string) string {
	if reason == "" || cfg.RunbookBaseURL == "" {
		return ""
	}
	return cfg.RunbookBaseURL + strings.ToLower(reason) + "/"
}
