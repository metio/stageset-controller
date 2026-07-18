// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/actionplan"
	"github.com/metio/stageset-controller/internal/inventory"
	"github.com/metio/stageset-controller/internal/preview"
	"github.com/metio/stageset-controller/internal/stageinv"
	"github.com/metio/stageset-controller/internal/window"
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
	renderedRefs := map[string][]inventory.ObjectRef{}
	for i := range selected {
		render, rerr := engine.RenderStage(ctx, &ss, &selected[i])
		if rerr != nil {
			return runtimeErr(rerr)
		}
		renders[selected[i].Name] = render
		refs := make([]inventory.ObjectRef, 0, len(render.Objects))
		for _, obj := range render.Objects {
			refs = append(refs, stageinv.RefOf(obj))
		}
		renderedRefs[selected[i].Name] = refs
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

	// Migrations the controller has queued for the version transition (from its
	// last-computed status). A pending migration is work the next reconcile runs.
	if migs := pendingMigrations(&ss); len(migs) > 0 {
		willRun = true
		fmt.Fprintln(out, "  migrations:")
		for _, m := range migs {
			verbs := ""
			if len(m.Actions) > 0 {
				verbs = "  [" + strings.Join(m.Actions, ", ") + "]"
			}
			fmt.Fprintf(out, "    %-24s %s → %s  before %s%s\n", m.Name, orNone(m.From), m.To, m.Stage, verbs)
		}
	}

	// Gates that would hold the rollout. The update window is recomputed now (a
	// pure function of the schedule and the clock); the promotion and error-budget
	// holds are read from status, so they reflect the last observed state.
	if gates := planGates(&ss, priorStages, selected, time.Now()); len(gates) > 0 {
		fmt.Fprintln(out, "  gates:")
		for _, g := range gates {
			fmt.Fprintf(out, "    %s\n", g)
		}
	}

	// Objects that would be pruned — in a stage's inventory but no longer in its
	// render. Pruning a state-bearing object (a PVC, a StatefulSet) destroys data,
	// so it is flagged: the "a prune deletes the database" hazard surfaced as a red
	// line here, not discovered as an incident.
	if prunes, perr := engine.PrunePlan(ctx, &ss, renderedRefs); perr != nil {
		fmt.Fprintf(out, "  note: prune preview unavailable: %v\n", perr)
	} else if len(prunes) > 0 {
		willRun = true
		fmt.Fprintln(out, "  prunes:")
		for _, it := range prunes {
			if isStateBearing(it.Ref) {
				fmt.Fprintf(out, "    ⚠ %s/%s  (stage %s) — deleting this destroys its data\n", it.Ref.Kind, it.Ref.Name, it.Stage)
			} else {
				fmt.Fprintf(out, "    %s/%s  (stage %s)\n", it.Ref.Kind, it.Ref.Name, it.Stage)
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

// planGates returns the gate-hold lines currently blocking the rollout: a closed
// update window (recomputed now — a pure function of the schedule and the clock),
// an error-budget freeze, and any stage awaiting promotion (both read from
// status). Each line is tagged to say whether it was recomputed or read live.
func planGates(ss *stagesv1.StageSet, priorStages map[string]stagesv1.StageStatus, selected []stagesv1.Stage, now time.Time) []string {
	var gates []string
	if allowed, nextChange, err := window.Decision(ss.Spec.UpdateWindows, now); err == nil && !allowed {
		opens := ""
		if !nextChange.IsZero() {
			opens = "; opens " + nextChange.Format(time.RFC3339)
		}
		gates = append(gates, fmt.Sprintf("update window  HOLD  (closed%s) [live: now]", opens))
	}
	if f := ss.Status.BudgetFreeze; f != nil {
		gates = append(gates, fmt.Sprintf("error budget   HOLD  (frozen; remaining %s, resumes at %s) [live: status]", orNone(f.Remaining), orNone(f.ResumeThreshold)))
	}
	for i := range selected {
		st := priorStages[selected[i].Name]
		if st.PromotionState != nil && st.PromotionState.Phase != "" {
			gates = append(gates, fmt.Sprintf("promotion (%s)  HOLD  (%s) [live: status]", selected[i].Name, st.PromotionState.Phase))
		}
		if st.BudgetFreeze != nil {
			gates = append(gates, fmt.Sprintf("error budget (%s)  HOLD  (frozen) [live: status]", selected[i].Name))
		}
	}
	return gates
}

// isStateBearing reports whether pruning a ref would destroy persistent data —
// the kinds where a prune is a data-loss event, not a routine cleanup.
func isStateBearing(ref inventory.ObjectRef) bool {
	switch {
	case ref.Group == "" && (ref.Kind == "PersistentVolumeClaim" || ref.Kind == "PersistentVolume"):
		return true
	case ref.Group == "apps" && ref.Kind == "StatefulSet":
		return true
	default:
		return false
	}
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}
