// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/actionplan"
	"github.com/metio/stageset-controller/internal/inventory"
	"github.com/metio/stageset-controller/internal/preview"
	"github.com/metio/stageset-controller/internal/stageinv"
	"github.com/metio/stageset-controller/internal/window"
)

type planOptions struct {
	names         []string
	stages        []string
	sourceDirs    []string
	output        string
	allNamespaces bool
	selector      string
}

// --- the plan data model (rendered as text, JSON, or YAML) ---

// stageSetPlan is one StageSet's predicted next reconcile.
type stageSetPlan struct {
	Namespace  string          `json:"namespace"`
	Name       string          `json:"name"`
	Version    *versionPlan    `json:"version,omitempty"`
	Stages     []stagePlan     `json:"stages,omitempty"`
	Migrations []migrationPlan `json:"migrations,omitempty"`
	Gates      []string        `json:"gates,omitempty"`
	Prunes     []prunePlan     `json:"prunes,omitempty"`
	Notes      []string        `json:"notes,omitempty"`
	// Error is set when this StageSet could not be planned (render failed, a
	// decryption key was unreadable). During a fan-out a broken StageSet reports
	// its error here and the rest are still planned, rather than aborting the run.
	Error string `json:"error,omitempty"`
	// WouldRun is true when the reconcile would run an action or migration, or
	// prune an object — the signal behind the exit code.
	WouldRun bool `json:"wouldRun"`
}

type versionPlan struct {
	Current string `json:"current,omitempty"`
	Desired string `json:"desired,omitempty"`
}

type stagePlan struct {
	Name     string          `json:"name"`
	Revision string          `json:"revision,omitempty"`
	Actions  []actionVerdict `json:"actions,omitempty"`
}

type actionVerdict struct {
	Phase  string `json:"phase"`
	Name   string `json:"name"`
	Scope  string `json:"scope"`
	State  string `json:"state"`
	Reason string `json:"reason"`
}

type migrationPlan struct {
	Name    string   `json:"name"`
	From    string   `json:"from,omitempty"`
	To      string   `json:"to"`
	Stage   string   `json:"stage"`
	Actions []string `json:"actions,omitempty"`
}

type prunePlan struct {
	Stage        string `json:"stage"`
	Kind         string `json:"kind"`
	Name         string `json:"name"`
	StateBearing bool   `json:"stateBearing"`
}

func newPlanCommand(o *options) *cobra.Command {
	opts := planOptions{}
	cmd := &cobra.Command{
		Use:   "plan [NAME...]",
		Short: "Preview what the next reconcile will do — which actions run, skip, or re-run",
		Long: "The behavioral sibling of `diff`: where `diff` shows which objects change, `plan` shows what the next " +
			"reconcile will DO — per stage, which pre/post actions will run, skip, or re-run and why, which migrations " +
			"are queued, what gate would hold the rollout, and which objects would be pruned. It predicts what will be " +
			"ATTEMPTED, in what order — never whether an action will succeed. Plan one StageSet by name, several by " +
			"name, or a whole fleet with --all-namespaces / --selector; -o json|yaml emits a machine-readable plan. It " +
			"reads the cluster but changes nothing; exit code 1 means at least one plan would run something.",
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.names = args
			return runPlan(cmd.Context(), o, opts)
		},
	}
	cmd.Flags().StringArrayVar(&opts.stages, "stage", nil, "Limit the plan to these stages (repeatable). Default: all.")
	cmd.Flags().StringArrayVar(&opts.sourceDirs, "source-dir", nil, "stage=DIR to render a stage from a local directory instead of its source (repeatable).")
	cmd.Flags().StringVarP(&opts.output, "output", "o", "text", "Output format: text, json, or yaml.")
	cmd.Flags().BoolVarP(&opts.allNamespaces, "all-namespaces", "A", false, "Plan every StageSet across all namespaces.")
	cmd.Flags().StringVarP(&opts.selector, "selector", "l", "", "Plan StageSets matching a label selector.")
	return cmd
}

func runPlan(ctx context.Context, o *options, opts planOptions) error {
	switch opts.output {
	case "text", "json", "yaml":
	default:
		return usageErr(fmt.Errorf("--output must be one of text, json, yaml"))
	}
	if len(opts.names) > 0 && (opts.allNamespaces || opts.selector != "") {
		return usageErr(fmt.Errorf("name arguments cannot be combined with --all-namespaces or --selector"))
	}
	sourceDirs, err := parseSourceDirs(opts.sourceDirs)
	if err != nil {
		return runtimeErr(err)
	}
	c, _, err := o.newClient()
	if err != nil {
		return runtimeErr(err)
	}

	targets, err := planTargets(ctx, c, o, opts)
	if err != nil {
		return runtimeErr(err)
	}
	if len(targets) == 0 {
		fmt.Fprintln(o.streams.ErrOut, "no StageSets matched")
		return nil
	}

	fanOut := len(opts.names) == 0
	plans := make([]stageSetPlan, 0, len(targets))
	anyWouldRun := false
	sawError := false
	for i := range targets {
		p, perr := computeStageSetPlan(ctx, o, c, &targets[i], opts.stages, sourceDirs)
		if perr != nil {
			// An explicitly named target surfaces its error directly; during a
			// fan-out one broken StageSet must not hide the rest, so its error is
			// recorded against it and the run continues.
			if !fanOut {
				return runtimeErr(perr)
			}
			plans = append(plans, stageSetPlan{Namespace: targets[i].Namespace, Name: targets[i].Name, Error: perr.Error()})
			sawError = true
			continue
		}
		plans = append(plans, *p)
		if p.WouldRun {
			anyWouldRun = true
		}
	}

	if err := renderPlans(o.streams.Out, plans, opts.output); err != nil {
		return runtimeErr(err)
	}
	// Exit code: a StageSet that could not be planned is a runtime failure (3) and
	// outranks a would-run plan; otherwise diff-style — 1 when at least one plan
	// would run something, 0 when none would.
	switch {
	case sawError:
		return &exitErr{code: exitError}
	case anyWouldRun:
		return &exitErr{code: exitDiff}
	default:
		return nil
	}
}

// planTargets resolves the StageSets to plan: the named ones, or — with no names
// — every StageSet in the namespace, all namespaces, or matching a selector.
func planTargets(ctx context.Context, c client.Client, o *options, opts planOptions) ([]stagesv1.StageSet, error) {
	if len(opts.names) > 0 {
		out := make([]stagesv1.StageSet, 0, len(opts.names))
		for _, name := range opts.names {
			var ss stagesv1.StageSet
			if err := c.Get(ctx, client.ObjectKey{Namespace: o.namespace(), Name: name}, &ss); err != nil {
				return nil, err
			}
			out = append(out, ss)
		}
		return out, nil
	}
	var listOpts []client.ListOption
	if !opts.allNamespaces {
		listOpts = append(listOpts, client.InNamespace(o.namespace()))
	}
	if opts.selector != "" {
		sel, err := labels.Parse(opts.selector)
		if err != nil {
			return nil, fmt.Errorf("invalid --selector: %w", err)
		}
		listOpts = append(listOpts, client.MatchingLabelsSelector{Selector: sel})
	}
	var list stagesv1.StageSetList
	if err := c.List(ctx, &list, listOpts...); err != nil {
		return nil, err
	}
	return list.Items, nil
}

// computeStageSetPlan renders one StageSet and predicts its next reconcile,
// returning the structured plan. It reads the cluster but mutates nothing.
func computeStageSetPlan(ctx context.Context, o *options, c client.Client, ss *stagesv1.StageSet, stages []string, sourceDirs map[string]string) (*stageSetPlan, error) {
	selected, err := preview.SelectStages(ss, stages)
	if err != nil {
		return nil, err
	}

	var lifetime *stagesv1.StageLedger
	var ledger stagesv1.StageLedger
	if lerr := c.Get(ctx, client.ObjectKey{Namespace: ss.Namespace, Name: ss.Name}, &ledger); lerr == nil {
		lifetime = &ledger
	} else if !apierrors.IsNotFound(lerr) {
		return nil, lerr
	}

	dec, derr := o.stageSetDecryptor(ctx, c, false, ss)
	if derr != nil {
		return nil, derr
	}
	engine := preview.NewEngine(c, false)
	engine.SourceDirs = sourceDirs
	engine.Decryptor = dec

	renders := make(map[string]preview.StageRender, len(selected))
	renderedRefs := map[string][]inventory.ObjectRef{}
	for i := range selected {
		render, rerr := engine.RenderStage(ctx, ss, &selected[i])
		if rerr != nil {
			return nil, rerr
		}
		renders[selected[i].Name] = render
		refs := make([]inventory.ObjectRef, 0, len(render.Objects))
		for _, obj := range render.Objects {
			refs = append(refs, stageinv.RefOf(obj))
		}
		renderedRefs[selected[i].Name] = refs
	}

	desired, versionNote := resolvePlanVersion(ss, renders)
	priorStages := indexStageStatuses(ss.Status.Stages)

	p := &stageSetPlan{Namespace: ss.Namespace, Name: ss.Name}
	if ss.Spec.Version != nil {
		p.Version = &versionPlan{Current: ss.Status.Version, Desired: desired}
	}

	anyLifetime := false
	for i := range selected {
		stage := &selected[i]
		sp := stagePlan{Name: stage.Name, Revision: renders[stage.Name].Revision}
		for _, v := range actionplan.ActionVerdicts(ctx, c, stage, actionplan.VerdictInputs{
			Namespace:      ss.Namespace,
			Revision:       renders[stage.Name].Revision,
			Versioned:      ss.Spec.Version != nil,
			DesiredVersion: desired,
			CurrentVersion: ss.Status.Version,
			Prior:          priorStages[stage.Name],
			Lifetime:       lifetime,
		}) {
			sp.Actions = append(sp.Actions, actionVerdict{
				Phase: v.Phase, Name: v.Name, Scope: string(v.Scope), State: string(v.State), Reason: v.Reason,
			})
			if v.State != actionplan.Skip {
				p.WouldRun = true
			}
			if v.Scope == stagesv1.ScopeLifetime {
				anyLifetime = true
			}
		}
		p.Stages = append(p.Stages, sp)
	}

	for _, m := range pendingMigrations(ss) {
		p.WouldRun = true
		p.Migrations = append(p.Migrations, migrationPlan{Name: m.Name, From: m.From, To: m.To, Stage: m.Stage, Actions: m.Actions})
	}

	p.Gates = planGates(ss, priorStages, selected, time.Now())

	if prunes, perr := engine.PrunePlan(ctx, ss, renderedRefs); perr != nil {
		p.Notes = append(p.Notes, "prune preview unavailable: "+perr.Error())
	} else {
		for _, it := range prunes {
			p.WouldRun = true
			p.Prunes = append(p.Prunes, prunePlan{Stage: it.Stage, Kind: it.Ref.Kind, Name: it.Ref.Name, StateBearing: isStateBearing(it.Ref)})
		}
	}

	if versionNote != "" {
		p.Notes = append(p.Notes, versionNote)
	}
	if anyLifetime {
		p.Notes = append(p.Notes, "scope: Lifetime results reflect the current cluster (completionAnchor witnesses are read live).")
	}
	return p, nil
}

func renderPlans(out io.Writer, plans []stageSetPlan, format string) error {
	switch format {
	case "json":
		b, err := json.MarshalIndent(plans, "", "  ")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, string(b))
		return err
	case "yaml":
		b, err := yaml.Marshal(plans)
		if err != nil {
			return err
		}
		_, err = out.Write(b)
		return err
	default:
		for i := range plans {
			renderPlanText(out, &plans[i])
		}
		return nil
	}
}

func renderPlanText(out io.Writer, p *stageSetPlan) {
	header := fmt.Sprintf("StageSet %s/%s", p.Namespace, p.Name)
	if p.Version != nil {
		header += fmt.Sprintf("  (version %s → %s)", orNone(p.Version.Current), orNone(p.Version.Desired))
	}
	fmt.Fprintln(out, header)
	if p.Error != "" {
		fmt.Fprintf(out, "  error: %s\n", p.Error)
		return
	}
	for _, s := range p.Stages {
		fmt.Fprintf(out, "  stage %s  (revision %s)\n", s.Name, orNone(s.Revision))
		for _, a := range s.Actions {
			fmt.Fprintf(out, "    %-4s %-24s %-8s (scope: %s — %s)\n", a.Phase, a.Name, a.State, a.Scope, a.Reason)
		}
	}
	if len(p.Migrations) > 0 {
		fmt.Fprintln(out, "  migrations:")
		for _, m := range p.Migrations {
			verbs := ""
			if len(m.Actions) > 0 {
				verbs = "  [" + strings.Join(m.Actions, ", ") + "]"
			}
			fmt.Fprintf(out, "    %-24s %s → %s  before %s%s\n", m.Name, orNone(m.From), m.To, m.Stage, verbs)
		}
	}
	if len(p.Gates) > 0 {
		fmt.Fprintln(out, "  gates:")
		for _, g := range p.Gates {
			fmt.Fprintf(out, "    %s\n", g)
		}
	}
	if len(p.Prunes) > 0 {
		fmt.Fprintln(out, "  prunes:")
		for _, pr := range p.Prunes {
			if pr.StateBearing {
				fmt.Fprintf(out, "    ⚠ %s/%s  (stage %s) — deleting this destroys its data\n", pr.Kind, pr.Name, pr.Stage)
			} else {
				fmt.Fprintf(out, "    %s/%s  (stage %s)\n", pr.Kind, pr.Name, pr.Stage)
			}
		}
	}
	for _, n := range p.Notes {
		fmt.Fprintf(out, "  note: %s\n", n)
	}
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
		idx, err := actionplan.VersionStageIndex(ss, v.FromArtifact.Stage)
		if err != nil {
			return "", "version.fromArtifact: " + err.Error() + "; version-scoped actions shown as would-run"
		}
		render, ok := renders[ss.Spec.Stages[idx].Name]
		if !ok {
			return "", "version.fromArtifact stage was not among the planned stages; version-scoped actions shown as would-run"
		}
		content, ok := render.Files[v.FromArtifact.Path]
		if !ok {
			return "", "version.fromArtifact: version file " + v.FromArtifact.Path + " not found in the stage source; version-scoped actions shown as would-run"
		}
		ver := strings.TrimSpace(content)
		if ver == "" {
			return "", "version.fromArtifact: version file " + v.FromArtifact.Path + " is empty; version-scoped actions shown as would-run"
		}
		return ver, ""
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
