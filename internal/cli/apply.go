// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"sigs.k8s.io/controller-runtime/pkg/client"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/apply"
	"github.com/metio/stageset-controller/internal/preview"
)

type applyOptions struct {
	name       string
	stages     []string
	sourceDirs []string
	asTenant   bool
	wait       bool
	timeout    time.Duration
}

func newApplyCommand(o *options) *cobra.Command {
	opts := applyOptions{timeout: 5 * time.Minute}
	cmd := &cobra.Command{
		Use:   "apply NAME",
		Short: "Apply a StageSet's rendered manifests to the cluster",
		Long: "Render a StageSet's stages with the controller's resolve→fetch→build path and server-side-apply the " +
			"resulting objects in stage order, under the controller's field manager and owner labels — so the controller " +
			"sees no drift afterward.\n\n" +
			"apply materializes the manifests only. It does NOT run stage actions, migrations, update-window gating, " +
			"ready checks (beyond --wait), or pruning — those belong to the controller's reconcile loop. When a " +
			"controller manages this StageSet it remains the source of truth and will reconcile on its own schedule; " +
			"apply is for cluster-free workflows and break-glass. Preview first with `diff`.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.name = args[0]
			return runApply(cmd.Context(), o, opts)
		},
	}
	cmd.Flags().StringSliceVar(&opts.stages, "stage", nil, "Apply only the named stage(s); repeatable.")
	cmd.Flags().StringArrayVar(&opts.sourceDirs, "source-dir", nil, "Local artifact tree as [STAGE=]PATH; repeatable. Skips the cluster fetch.")
	cmd.Flags().BoolVar(&opts.asTenant, "as-tenant", false, "Render and apply each stage as its effective serviceAccountName (the stage's own, else spec.serviceAccountName).")
	cmd.Flags().BoolVar(&opts.wait, "wait", false, "Wait for each stage's objects to become ready before applying the next stage.")
	cmd.Flags().DurationVar(&opts.timeout, "timeout", 5*time.Minute, "Per-stage readiness timeout with --wait.")
	return cmd
}

func runApply(ctx context.Context, o *options, opts applyOptions) error {
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

	// spec.decryption: decrypt where the controller does, or the apply below
	// would SSA ciphertext the controller immediately rewrites decrypted.
	dec, derr := o.stageSetDecryptor(ctx, c, opts.asTenant, &ss)
	if derr != nil {
		return runtimeErr(derr)
	}

	// Each stage renders and applies under its effective ServiceAccount (its own
	// serviceAccountName, else the StageSet default), mirroring the controller.
	// Without --as-tenant every stage uses the CLI's own credentials. Runtimes are
	// cached per effective SA — impersonatedClient builds a discovery mapper, so a
	// StageSet whose stages share an SA pays that cost once. The RESTMapper is
	// identity-independent, so the base mapper is reused for every applier.
	type applyRuntime struct {
		engine  *preview.Engine
		applier *apply.Applier
	}
	runtimes := map[string]applyRuntime{}
	runtimeFor := func(sa string) (applyRuntime, error) {
		key := ""
		applyClient := c
		if opts.asTenant && sa != "" {
			key = sa
			if rt, ok := runtimes[key]; ok {
				return rt, nil
			}
			ic, ierr := o.impersonatedClient(ss.Namespace, sa)
			if ierr != nil {
				return applyRuntime{}, ierr
			}
			applyClient = ic
		} else if rt, ok := runtimes[key]; ok {
			return rt, nil
		}
		engine := preview.NewEngine(applyClient, false)
		engine.SourceDirs = sourceDirs
		engine.Decryptor = dec
		rt := applyRuntime{engine: engine, applier: apply.New(applyClient, mapper, stagesv1.GroupVersion.Group)}
		runtimes[key] = rt
		return rt, nil
	}

	total := 0
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
		engine, applier := rt.engine, rt.applier
		render, rerr := engine.RenderStage(ctx, &ss, stage)
		if rerr != nil {
			return runtimeErr(rerr)
		}
		// Stamp the per-stage discovery label exactly as a reconcile does, so the
		// applied objects are indistinguishable from controller-applied ones and a
		// later reconcile sees no drift.
		apply.StampStageLabel(render.Objects, stagesv1.StageLabel, stage.Name)

		// Resolve the stage's conflictPolicy/force exactly as a reconcile does, so
		// `stagesetctl apply` recreates / keeps objects the same way the controller
		// would instead of erroring on an immutable-field conflict.
		ch, cerr := apply.ResolveConflictHandling(render.Objects, stage, apply.NewForceToken())
		if cerr != nil {
			return runtimeErr(fmt.Errorf("stage %q conflict policy: %w", stage.Name, cerr))
		}
		changeSet, aerr := applier.Apply(ctx, ss.Name, ss.Namespace, render.Objects, ch)
		if aerr != nil {
			return runtimeErr(fmt.Errorf("apply stage %q: %w", stage.Name, aerr))
		}

		fmt.Fprintf(o.streams.Out, "stage %q:\n", stage.Name)
		for _, e := range changeSet.Entries {
			fmt.Fprintf(o.streams.Out, "  %s %s\n", e.Action, e.Subject)
			total++
		}

		if opts.wait {
			if werr := applier.Wait(ctx, changeSet.ToObjMetadataSet(), opts.timeout); werr != nil {
				return runtimeErr(fmt.Errorf("wait for stage %q: %w", stage.Name, werr))
			}
		}
	}

	fmt.Fprintf(o.streams.Out, "applied %d object(s) across %d stage(s)\n", total, len(selected))
	return nil
}
