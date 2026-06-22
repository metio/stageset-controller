/*
 * SPDX-FileCopyrightText: The stageset-controller Authors
 * SPDX-License-Identifier: 0BSD
 */

package mcp

import (
	"context"
	"fmt"
	"time"

	fluxmeta "github.com/fluxcd/pkg/apis/meta"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"sigs.k8s.io/controller-runtime/pkg/client"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

type mutateInput struct {
	Namespace string `json:"namespace" jsonschema:"the StageSet's namespace"`
	Name      string `json:"name" jsonschema:"the StageSet's name"`
}

type mutateOutput struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Result    string `json:"result" jsonschema:"a short description of what changed"`
}

// registerMutationTools wires the gated write tools. Only called when the server
// has a client AND mutations are explicitly enabled (--mcp-allow-mutations).
func registerMutationTools(server *mcpsdk.Server, cfg Config) {
	mcpsdk.AddTool(server, &mcpsdk.Tool{
		Name:        "reconcile_stageset",
		Description: "Request an immediate reconcile of a StageSet by stamping its reconcile.fluxcd.io/requestedAt annotation — the same trigger as `flux reconcile`.",
	}, cfg.reconcileStageSetHandler)

	mcpsdk.AddTool(server, &mcpsdk.Tool{
		Name:        "suspend_stageset",
		Description: "Pause reconciliation of a StageSet by setting spec.suspend=true. The controller stops reconciling it until it is resumed.",
	}, cfg.suspendStageSetHandler)

	mcpsdk.AddTool(server, &mcpsdk.Tool{
		Name:        "resume_stageset",
		Description: "Resume reconciliation of a suspended StageSet by clearing spec.suspend.",
	}, cfg.resumeStageSetHandler)
}

func (cfg Config) reconcileStageSetHandler(ctx context.Context, _ *mcpsdk.CallToolRequest, in mutateInput) (*mcpsdk.CallToolResult, mutateOutput, error) {
	return cfg.mutateStageSet(ctx, in, func(s *stagesv1.StageSet) (string, bool) {
		// A suspended StageSet is short-circuited before the controller ever reads
		// the reconcile annotation, so stamping it would report success for a
		// guaranteed no-op. Mirror the CLI's refusal instead of misleading the
		// agent.
		if s.Spec.Suspend {
			return "StageSet is suspended; the controller will not act on a reconcile request until it is resumed", false
		}
		if s.Annotations == nil {
			s.Annotations = map[string]string{}
		}
		token := time.Now().UTC().Format(time.RFC3339Nano)
		s.Annotations[fluxmeta.ReconcileRequestAnnotation] = token
		return "reconcile requested at " + token, true
	})
}

func (cfg Config) suspendStageSetHandler(ctx context.Context, _ *mcpsdk.CallToolRequest, in mutateInput) (*mcpsdk.CallToolResult, mutateOutput, error) {
	return cfg.mutateStageSet(ctx, in, func(s *stagesv1.StageSet) (string, bool) {
		if s.Spec.Suspend {
			return "already suspended", false
		}
		s.Spec.Suspend = true
		return "suspended", true
	})
}

func (cfg Config) resumeStageSetHandler(ctx context.Context, _ *mcpsdk.CallToolRequest, in mutateInput) (*mcpsdk.CallToolResult, mutateOutput, error) {
	return cfg.mutateStageSet(ctx, in, func(s *stagesv1.StageSet) (string, bool) {
		if !s.Spec.Suspend {
			return "not suspended; no change", false
		}
		s.Spec.Suspend = false
		return "resumed", true
	})
}

// mutateStageSet Gets the StageSet, applies mutate (which returns a result
// description and whether anything changed), and Patches only when something
// changed. The patch is a MergeFrom diff so concurrent status writes by the
// controller don't conflict with a spec/annotation change.
func (cfg Config) mutateStageSet(ctx context.Context, in mutateInput, mutate func(*stagesv1.StageSet) (string, bool)) (*mcpsdk.CallToolResult, mutateOutput, error) {
	if in.Namespace == "" || in.Name == "" {
		return errorResult("both namespace and name are required"), mutateOutput{}, nil
	}
	var ss stagesv1.StageSet
	if err := cfg.KubeClient.Get(ctx, client.ObjectKey{Namespace: in.Namespace, Name: in.Name}, &ss); err != nil {
		return errorResult(fmt.Sprintf("cannot get StageSet %s/%s: %v", in.Namespace, in.Name, err)), mutateOutput{}, nil
	}
	before := ss.DeepCopy()
	desc, changed := mutate(&ss)
	if changed {
		if err := cfg.KubeClient.Patch(ctx, &ss, client.MergeFrom(before)); err != nil {
			return errorResult(fmt.Sprintf("cannot update StageSet %s/%s: %v", in.Namespace, in.Name, err)), mutateOutput{}, nil
		}
	}
	return nil, mutateOutput{Namespace: in.Namespace, Name: in.Name, Result: desc}, nil
}
