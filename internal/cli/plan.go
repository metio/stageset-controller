// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/actionplan"
	"github.com/metio/stageset-controller/internal/preview"
)

type planOptions struct {
	name       string
	stages     []string
	sourceDirs []string
}

func newPlanCommand(o *options) *cobra.Command {
	opts := planOptions{}
	cmd := &cobra.Command{
		Use:   "plan NAME",
		Short: "Preview what the next reconcile will do — which actions run, skip, or re-run",
		Long: "The behavioral sibling of `diff`: where `diff` shows which objects change, `plan` shows what the next " +
			"reconcile will DO — per stage, which pre/post actions will run, skip, or re-run, and why. It predicts " +
			"what will be ATTEMPTED, in what order, under which scope — never whether an action will succeed. It reads " +
			"the cluster (ledgers, completionAnchor witnesses) but changes nothing; exit code 1 means the reconcile " +
			"would run at least one action.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.name = args[0]
			return runPlan(cmd.Context(), o, opts)
		},
	}
	cmd.Flags().StringArrayVar(&opts.stages, "stage", nil, "Limit the plan to these stages (repeatable). Default: all.")
	cmd.Flags().StringArrayVar(&opts.sourceDirs, "source-dir", nil, "stage=DIR to render a stage from a local directory instead of its source (repeatable).")
	return cmd
}

func runPlan(ctx context.Context, o *options, opts planOptions) error {
	sourceDirs, err := parseSourceDirs(opts.sourceDirs)
	if err != nil {
		return runtimeErr(err)
	}
	c, _, err := o.newClient()
	if err != nil {
		return runtimeErr(err)
	}
	var ss stagesv1.StageSet
	if err := c.Get(ctx, client.ObjectKey{Namespace: o.namespace(), Name: opts.name}, &ss); err != nil {
		return runtimeErr(err)
	}
	selected, err := preview.SelectStages(&ss, opts.stages)
	if err != nil {
		return runtimeErr(err)
	}

	// The StageLedger gates scope: Lifetime actions; read it under the caller's own
	// credentials (absent means nothing recorded yet).
	var lifetime *stagesv1.StageLedger
	var ledger stagesv1.StageLedger
	if lerr := c.Get(ctx, client.ObjectKey{Namespace: ss.Namespace, Name: ss.Name}, &ledger); lerr == nil {
		lifetime = &ledger
	} else if !apierrors.IsNotFound(lerr) {
		return runtimeErr(lerr)
	}

	dec, derr := o.stageSetDecryptor(ctx, c, false, &ss)
	if derr != nil {
		return runtimeErr(derr)
	}
	engine := preview.NewEngine(c, false)
	engine.SourceDirs = sourceDirs
	engine.Decryptor = dec

	renders := make(map[string]preview.StageRender, len(selected))
	for i := range selected {
		render, rerr := engine.RenderStage(ctx, &ss, &selected[i])
		if rerr != nil {
			return runtimeErr(rerr)
		}
		renders[selected[i].Name] = render
	}

	desired, versionNote := resolvePlanVersion(&ss, renders)
	priorStages := indexStageStatuses(ss.Status.Stages)

	out := o.streams.Out
	header := fmt.Sprintf("StageSet %s/%s", ss.Namespace, ss.Name)
	if ss.Spec.Version != nil {
		header += fmt.Sprintf("  (version %s → %s)", orNone(ss.Status.Version), orNone(desired))
	}
	fmt.Fprintln(out, header)

	anyLifetime := false
	willRun := false
	for i := range selected {
		stage := &selected[i]
		render := renders[stage.Name]
		fmt.Fprintf(out, "  stage %s  (revision %s)\n", stage.Name, orNone(render.Revision))
		verdicts := actionplan.ActionVerdicts(ctx, c, stage, actionplan.VerdictInputs{
			Namespace:      ss.Namespace,
			Revision:       render.Revision,
			Versioned:      ss.Spec.Version != nil,
			DesiredVersion: desired,
			CurrentVersion: ss.Status.Version,
			Prior:          priorStages[stage.Name],
			Lifetime:       lifetime,
		})
		for _, v := range verdicts {
			fmt.Fprintf(out, "    %-4s %-24s %-8s (scope: %s — %s)\n", v.Phase, v.Name, v.State, v.Scope, v.Reason)
			if v.State != actionplan.Skip {
				willRun = true
			}
			if v.Scope == stagesv1.ScopeLifetime {
				anyLifetime = true
			}
		}
	}

	if versionNote != "" {
		fmt.Fprintf(out, "  note: %s\n", versionNote)
	}
	if anyLifetime {
		fmt.Fprintln(out, "  note: scope: Lifetime results reflect the current cluster (completionAnchor witnesses are read live).")
	}

	// diff-style exit code: 1 when the reconcile would run at least one action.
	if willRun {
		return &exitErr{code: exitDiff}
	}
	return nil
}

// resolvePlanVersion resolves spec.version for the preview from an inline value or
// a field of a rendered object; it returns the version and, when it cannot
// resolve one, a note explaining why (so version-scoped verdicts are read with
// that caveat). It never reads the live deployed object — fromObject reads the
// version off the freshly rendered manifests, the same object the reconciler
// builds — so the result is reproducible from the source.
func resolvePlanVersion(ss *stagesv1.StageSet, renders map[string]preview.StageRender) (string, string) {
	v := ss.Spec.Version
	switch {
	case v == nil:
		return "", ""
	case v.Value != "":
		return v.Value, ""
	case v.FromObject != nil:
		idx, err := actionplan.VersionStageIndex(ss, v.FromObject.Stage)
		if err != nil {
			return "", "version.fromObject: " + err.Error() + "; version-scoped actions shown as would-run"
		}
		render, ok := renders[ss.Spec.Stages[idx].Name]
		if !ok {
			return "", "version.fromObject stage was not among the planned stages; version-scoped actions shown as would-run"
		}
		obj := actionplan.FindVersionObject(render.Objects, v.FromObject)
		if obj == nil {
			return "", "version.fromObject: version object not found in the render; version-scoped actions shown as would-run"
		}
		ver, err := actionplan.ExtractVersionField(obj, v.FromObject.FieldPath)
		if err != nil {
			return "", "version.fromObject: " + err.Error() + "; version-scoped actions shown as would-run"
		}
		return ver, ""
	default: // FromArtifact
		return "", "version.fromArtifact is not resolved in the preview; version-scoped actions shown as would-run"
	}
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}
