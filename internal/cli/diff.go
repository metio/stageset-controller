// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package cli

import (
	"context"
	"fmt"
	"sort"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/actionplan"
	"github.com/metio/stageset-controller/internal/apply"
	"github.com/metio/stageset-controller/internal/diffrender"
	"github.com/metio/stageset-controller/internal/inventory"
	"github.com/metio/stageset-controller/internal/preview"
	"github.com/metio/stageset-controller/internal/stageinv"
)

type diffOptions struct {
	name             string
	stages           []string
	sourceDirs       []string
	serverSide       bool
	asTenant         bool
	noCrossNamespace bool
	showSecrets      bool
	showUnchanged    bool
	prune            bool
	color            string
	exitCode         bool
}

func newDiffCommand(o *options) *cobra.Command {
	opts := diffOptions{serverSide: true, prune: true, exitCode: true, color: "auto"}
	cmd := &cobra.Command{
		Use:   "diff NAME",
		Short: "Preview what a StageSet would change in the cluster",
		Long: "Render a StageSet's stages and show, per object, what a reconcile would create, configure, or delete — " +
			"including resources pruned because they fell out of the inventory. By default the diff is a server-side " +
			"dry-run apply (webhook- and defaulting-faithful) using the controller's field manager. Secret values are " +
			"masked unless --show-secrets is given. Exit code is 0 when clean, 1 when changes are found, >2 on error.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.name = args[0]
			return runDiff(cmd.Context(), o, opts)
		},
	}
	cmd.Flags().StringSliceVar(&opts.stages, "stage", nil, "Diff only the named stage(s); repeatable.")
	cmd.Flags().StringArrayVar(&opts.sourceDirs, "source-dir", nil, "Local artifact tree as [STAGE=]PATH; repeatable. Skips the cluster fetch.")
	cmd.Flags().BoolVar(&opts.serverSide, "server-side", true, "Server-side dry-run apply diff (needs update/patch RBAC). False compares the render against live objects client-side.")
	cmd.Flags().BoolVar(&opts.asTenant, "as-tenant", false, "Server-side dry-run each stage as its effective serviceAccountName (the stage's own, else spec.serviceAccountName) — the identity the controller applies with. Reads (source resolve, substituteFrom, inventory) always use your credentials, as the controller reads as itself.")
	cmd.Flags().BoolVar(&opts.noCrossNamespace, "no-cross-namespace-refs", false, "Match a controller run with --no-cross-namespace-refs: reject a stage sourceRef that targets another namespace, so the preview fails the same way the controller would.")
	cmd.Flags().BoolVar(&opts.showSecrets, "show-secrets", false, "Reveal Secret values instead of masking them.")
	cmd.Flags().BoolVar(&opts.showUnchanged, "show-unchanged", false, "Include objects with no change.")
	cmd.Flags().BoolVar(&opts.prune, "prune", true, "Show resources that would be deleted because they fell out of the inventory.")
	cmd.Flags().StringVar(&opts.color, "color", "auto", "Colorize output: auto, always, or never.")
	cmd.Flags().BoolVar(&opts.exitCode, "exit-code", true, "Exit 1 when changes are found. False always exits 0 on a clean run.")
	return cmd
}

func runDiff(ctx context.Context, o *options, opts diffOptions) error {
	color, err := colorEnabled(opts.color, o.streams.Out)
	if err != nil {
		// An unrecognized --color value is flag misuse, not a runtime failure.
		return usageErr(err)
	}
	sourceDirs, err := parseSourceDirs(opts.sourceDirs)
	if err != nil {
		return runtimeErr(err)
	}

	c, mapper, err := o.newClient()
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

	// spec.decryption: decrypt where the controller does, or the server-side
	// dry-run below would diff ciphertext against the controller's plaintext
	// and permanently report drift.
	dec, derr := o.stageSetDecryptor(ctx, c, opts.asTenant, &ss)
	if derr != nil {
		return runtimeErr(derr)
	}

	// Each stage renders and dry-run-diffs under its effective ServiceAccount (its
	// own serviceAccountName, else the StageSet default), mirroring the controller,
	// so the preview reflects what that stage's identity can read and write.
	// Without --as-tenant every stage uses the CLI's own credentials. Runtimes are
	// cached per effective SA; the RESTMapper is identity-independent so the base
	// mapper is reused. The cross-stage prune plan reads under the default runtime.
	type diffRuntime struct {
		engine      *preview.Engine
		applier     *apply.Applier
		applyClient client.Client
	}
	runtimes := map[string]diffRuntime{}
	runtimeFor := func(sa string) (diffRuntime, error) {
		key := ""
		applyClient := c
		if opts.asTenant && sa != "" {
			key = sa
			if rt, ok := runtimes[key]; ok {
				return rt, nil
			}
			ic, ierr := o.impersonatedClient(ss.Namespace, sa)
			if ierr != nil {
				return diffRuntime{}, ierr
			}
			applyClient = ic
		} else if rt, ok := runtimes[key]; ok {
			return rt, nil
		}
		// The ENGINE (source resolve, postBuild substituteFrom, inventory
		// reads) always runs with the caller's own credentials — the
		// controller performs those reads as itself, and spec.serviceAccountName
		// scopes only the stage's CLUSTER OPERATIONS (apply, prune, verify,
		// actions). Impersonating the reads would demand RBAC the documented
		// tenant role never grants and fail a diff the controller performs
		// fine. Only the server-side dry-run (the apply mirror) impersonates.
		engine := preview.NewEngine(c, opts.noCrossNamespace)
		engine.SourceDirs = sourceDirs
		engine.Decryptor = dec
		rt := diffRuntime{engine: engine, applier: apply.New(applyClient, mapper, stagesv1.GroupVersion.Group), applyClient: applyClient}
		runtimes[key] = rt
		return rt, nil
	}
	// The default runtime (StageSet-level SA) backs the cross-stage prune plan.
	defaultRT, err := runtimeFor(ss.Spec.ServiceAccountName)
	if err != nil {
		return runtimeErr(err)
	}

	// The StageLedger records scope: Lifetime completions. A once-ever action
	// already recorded (with a valid anchor) will not run, so the action preview
	// must not list it — the same gate the controller applies. Read under the
	// caller's own credentials; absent means nothing recorded yet.
	var lifetimeLedger *stagesv1.StageLedger
	var ledger stagesv1.StageLedger
	if lerr := c.Get(ctx, client.ObjectKey{Namespace: ss.Namespace, Name: ss.Name}, &ledger); lerr == nil {
		lifetimeLedger = &ledger
	} else if !apierrors.IsNotFound(lerr) {
		return runtimeErr(lerr)
	}

	priorStages := indexStageStatuses(ss.Status.Stages)
	diffByStage := map[string][]diffrender.Change{}
	renderedRefs := map[string][]inventory.ObjectRef{}
	var actions []diffrender.ActionPreview
	for i := range selected {
		stage := &selected[i]
		sa := stage.ServiceAccountName
		if sa == "" {
			sa = ss.Spec.ServiceAccountName
		}
		rt, rtErr := runtimeFor(sa)
		if rtErr != nil {
			return runtimeErr(rtErr)
		}
		engine, applier, applyClient := rt.engine, rt.applier, rt.applyClient
		render, rerr := engine.RenderStage(ctx, &ss, stage)
		if rerr != nil {
			return runtimeErr(rerr)
		}
		// Mirror the reconcile apply: the controller stamps the per-stage discovery
		// label on every applied object, so a faithful dry-run diff must too —
		// otherwise the label on a live object reads as a spurious "configure".
		apply.StampStageLabel(render.Objects, stagesv1.StageLabel, stage.Name)
		refs := make([]inventory.ObjectRef, 0, len(render.Objects))
		for _, obj := range render.Objects {
			refs = append(refs, stageinv.RefOf(obj))
		}
		renderedRefs[stage.Name] = refs

		changes, cerr := stageChanges(ctx, applier, applyClient, &ss, stage.Name, render.Objects, opts.serverSide)
		if cerr != nil {
			return runtimeErr(cerr)
		}
		diffByStage[stage.Name] = changes
		actions = append(actions, stageActionsToRun(ctx, c, ss.Namespace, stage, render.Revision, priorStages[stage.Name], lifetimeLedger)...)
	}

	pruneByStage := map[string][]diffrender.Change{}
	if opts.prune {
		items, perr := defaultRT.engine.PrunePlan(ctx, &ss, renderedRefs)
		if perr != nil {
			return runtimeErr(perr)
		}
		for _, it := range items {
			pruneByStage[it.Stage] = append(pruneByStage[it.Stage], pruneChange(it))
		}
	}

	changes := assembleChanges(selected, diffByStage, pruneByStage)

	masker := diffrender.NewSecretMasker(opts.showSecrets)
	sum, err := diffrender.RenderDiff(o.streams.Out, changes, diffrender.RenderOptions{
		ShowUnchanged: opts.showUnchanged,
		Color:         color,
		Masker:        masker,
	})
	if err != nil {
		return runtimeErr(err)
	}

	diffrender.WriteActions(o.streams.Out, actions, color)
	diffrender.WriteMigrations(o.streams.Out, pendingMigrations(&ss), color)
	diffrender.WriteSummary(o.streams.Out, sum)

	if sum.Changed() && opts.exitCode {
		return &exitErr{code: exitDiff}
	}
	return nil
}

// indexStageStatuses maps a StageSet's per-stage status by stage name, so the
// diff can tell which actions the idempotency ledger has already satisfied.
func indexStageStatuses(stages []stagesv1.StageStatus) map[string]stagesv1.StageStatus {
	out := make(map[string]stagesv1.StageStatus, len(stages))
	for _, s := range stages {
		out[s.Name] = s
	}
	return out
}

// stageActionsToRun lists the actions a stage would run on the next reconcile,
// omitting those a ledger already satisfies. A Revision-scoped action is omitted
// when the revision ledger recorded it at the rendered revision; a scope:
// Lifetime action is omitted when the StageLedger records it complete (with a
// valid anchor) — the same gate the controller applies, so the preview does not
// list a once-ever bootstrap that has already run. A local render (no revision)
// cannot consult the revision ledger, so those actions are all listed.
//
// scope: Version actions are not yet gated here — that needs the resolved
// version, which the diff does not compute; a held version action can still show
// as pending until the version-aware preview lands.
func stageActionsToRun(ctx context.Context, reader client.Client, ns string, stage *stagesv1.Stage, revision string, prior stagesv1.StageStatus, lifetime *stagesv1.StageLedger) []diffrender.ActionPreview {
	if stage.Actions == nil {
		return nil
	}
	executed := map[string]bool{}
	if revision != "" && prior.LedgerRevision == revision {
		for _, name := range prior.ExecutedActions {
			executed[name] = true
		}
	}
	scopes := actionplan.ActionScopes(stage)
	lifetimeDone := map[string]bool{}
	for _, name := range actionplan.EvaluateLifetimeGate(ctx, reader, lifetime, ns, stage.Name).Done {
		lifetimeDone[name] = true
	}
	var out []diffrender.ActionPreview
	add := func(phase string, list []stagesv1.Action) {
		for i := range list {
			name := list[i].Name
			if executed[name] {
				continue
			}
			if scopes[name] == stagesv1.ScopeLifetime && lifetimeDone[name] {
				continue // once-ever, already recorded complete
			}
			kind, detail := describeAction(&list[i])
			out = append(out, diffrender.ActionPreview{
				Stage: stage.Name, Phase: phase, Name: name, Type: kind, Detail: detail,
			})
		}
	}
	add("pre", stage.Actions.Pre)
	add("post", stage.Actions.Post)
	add("onFailure", stage.Actions.OnFailure)
	return out
}

// describeAction returns the action's type and a short, human-readable detail.
func describeAction(a *stagesv1.Action) (kind, detail string) {
	switch {
	case a.Patch != nil:
		t := a.Patch.Target
		if t.Name == "" && t.Selector != nil {
			return "patch", fmt.Sprintf("%s (selector)", t.Kind)
		}
		return "patch", fmt.Sprintf("%s/%s", t.Kind, t.Name)
	case a.HTTP != nil:
		method := a.HTTP.Method
		if method == "" {
			method = "POST"
		}
		return "http", method + " " + a.HTTP.URL
	case a.Wait != nil:
		if a.Wait.Expr != "" {
			return "wait", "expr"
		}
		if a.Wait.Duration != nil {
			return "wait", a.Wait.Duration.Duration.String()
		}
		return "wait", ""
	case a.Job != nil:
		return "job", a.Job.SourceRef.Name
	case a.Delete != nil:
		return "delete", fmt.Sprintf("%s/%s", a.Delete.Target.Kind, a.Delete.Target.Name)
	case a.Apply != nil:
		return "apply", a.Apply.SourceRef.Name
	default:
		return "action", ""
	}
}

// pendingMigrations maps the controller-computed status.pendingMigrations to
// previews directly — the rich status already carries the boundary, resolved
// anchor stage, and action verbs, so a sourced ladder (whose content is not in
// the spec) previews just as fully as an inline one.
func pendingMigrations(ss *stagesv1.StageSet) []diffrender.MigrationPreview {
	if len(ss.Status.PendingMigrations) == 0 {
		return nil
	}
	out := make([]diffrender.MigrationPreview, 0, len(ss.Status.PendingMigrations))
	for _, m := range ss.Status.PendingMigrations {
		out = append(out, diffrender.MigrationPreview{
			Name: m.Name, To: m.To, From: m.From, Stage: m.Stage, Actions: m.Actions,
		})
	}
	return out
}

// stageChanges turns a stage's rendered objects into per-object Changes, via a
// server-side dry-run apply (default) or a client-side render-vs-live compare.
func stageChanges(ctx context.Context, applier *apply.Applier, c client.Client, ss *stagesv1.StageSet, stage string, objects []*unstructured.Unstructured, serverSide bool) ([]diffrender.Change, error) {
	if serverSide {
		// Resolve the stage's conflictPolicy/force exactly as a reconcile would,
		// so the dry-run is faithful: without it a force/Recreate stage would
		// hard-error on an immutable-field conflict the reconcile would force
		// past, and a KeepExisting object would show as a change. ResolveConflictHandling
		// stamps the same marker annotations the reconcile stamps.
		ch := apply.ConflictHandling{}
		if st := specStage(ss, stage); st != nil {
			resolved, rerr := apply.ResolveConflictHandling(objects, st, apply.NewForceToken())
			if rerr != nil {
				return nil, rerr
			}
			ch = resolved
		}
		entries, err := applier.Diff(ctx, ss.Name, ss.Namespace, objects, ch)
		if err != nil {
			return nil, err
		}
		out := make([]diffrender.Change, 0, len(entries))
		for _, e := range entries {
			out = append(out, diffrender.Change{
				Stage:     stage,
				Kind:      diffKind(e.Action),
				GVK:       e.GVK,
				Namespace: e.Namespace,
				Name:      e.Name,
				Before:    e.Existing,
				After:     e.Merged,
			})
		}
		return out, nil
	}
	return clientSideChanges(ctx, c, stage, objects)
}

// clientSideChanges compares each rendered object against its live counterpart
// without a dry-run apply. Less faithful — it cannot show mutating-webhook or
// apiserver-defaulting effects — but needs only read access.
func clientSideChanges(ctx context.Context, c client.Client, stage string, objects []*unstructured.Unstructured) ([]diffrender.Change, error) {
	out := make([]diffrender.Change, 0, len(objects))
	for _, obj := range objects {
		live := &unstructured.Unstructured{}
		live.SetGroupVersionKind(obj.GroupVersionKind())
		err := c.Get(ctx, client.ObjectKeyFromObject(obj), live)
		ch := diffrender.Change{Stage: stage, GVK: obj.GroupVersionKind(), Namespace: obj.GetNamespace(), Name: obj.GetName(), After: obj}
		switch {
		case apierrors.IsNotFound(err):
			ch.Kind = diffrender.ChangeCreate
		case err != nil:
			return nil, err
		default:
			ch.Before = live
			if equalRender(live, obj) {
				ch.Kind = diffrender.ChangeUnchanged
				ch.Before, ch.After = nil, nil
			} else {
				ch.Kind = diffrender.ChangeConfigure
			}
		}
		out = append(out, ch)
	}
	return out, nil
}

// equalRender reports whether two objects render identically after noise is
// stripped — the client-side notion of "unchanged".
func equalRender(a, b *unstructured.Unstructured) bool {
	ac, bc := a.DeepCopy(), b.DeepCopy()
	diffrender.StripNoise(ac)
	diffrender.StripNoise(bc)
	ay, _ := diffrender.ToYAML(ac)
	by, _ := diffrender.ToYAML(bc)
	return string(ay) == string(by)
}

func pruneChange(it preview.PruneItem) diffrender.Change {
	gvk := schema.GroupVersionKind{Group: it.Ref.Group, Version: it.Ref.Version, Kind: it.Ref.Kind}
	return diffrender.Change{
		Stage:     it.Stage,
		Kind:      diffrender.ChangeDelete,
		GVK:       gvk,
		Namespace: it.Ref.Namespace,
		Name:      it.Ref.Name,
		Before:    it.Object,
	}
}

// assembleChanges orders output by spec stage order — each stage's diffs then
// its deletions — with deletions for removed stages (not in the selected set)
// emitted last.
func assembleChanges(selected []stagesv1.Stage, diffByStage, pruneByStage map[string][]diffrender.Change) []diffrender.Change {
	var changes []diffrender.Change
	emitted := map[string]bool{}
	for i := range selected {
		name := selected[i].Name
		changes = append(changes, diffByStage[name]...)
		changes = append(changes, pruneByStage[name]...)
		emitted[name] = true
	}
	var leftover []string
	for stage := range pruneByStage {
		if !emitted[stage] {
			leftover = append(leftover, stage)
		}
	}
	sort.Strings(leftover)
	for _, stage := range leftover {
		changes = append(changes, pruneByStage[stage]...)
	}
	return changes
}

func diffKind(a apply.DiffAction) diffrender.ChangeKind {
	switch a {
	case apply.DiffCreate:
		return diffrender.ChangeCreate
	case apply.DiffConfigure:
		return diffrender.ChangeConfigure
	case apply.DiffSkipped:
		return diffrender.ChangeSkip
	default:
		return diffrender.ChangeUnchanged
	}
}
