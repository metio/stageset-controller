// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package cli

import (
	"context"
	"fmt"
	"slices"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

// --- baseline ---

type baselineOptions struct {
	name   string
	stage  string
	action string
	export bool
}

func newBaselineCommand(o *options) *cobra.Command {
	opts := baselineOptions{}
	cmd := &cobra.Command{
		Use:   "baseline NAME --stage STAGE --action ACTION",
		Short: "Assert a scope: Lifetime action already completed, or export current completions",
		Long: "Adopt a system whose once-per-lifetime bootstrap already ran: assert an action as already " +
			"complete (without running it) by adding it to the StageLedger's spec.baseline, which the controller " +
			"promotes to a recorded completion. --export instead emits the ledger's current completions as a " +
			"committable spec.baseline snippet — the disaster-recovery and rename story, so a cluster rebuilt from " +
			"Git does not re-run a bootstrap whose effect survived.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.name = args[0]
			if opts.export {
				if opts.stage != "" || opts.action != "" {
					return usageErr(fmt.Errorf("--export cannot be combined with --stage/--action"))
				}
				return runtimeErr(runBaselineExport(cmd.Context(), o, opts))
			}
			if opts.stage == "" || opts.action == "" {
				return usageErr(fmt.Errorf("--stage and --action are required (or use --export)"))
			}
			return runtimeErr(runBaselineAdd(cmd.Context(), o, opts))
		},
	}
	cmd.Flags().StringVar(&opts.stage, "stage", "", "Stage the action belongs to.")
	cmd.Flags().StringVar(&opts.action, "action", "", "Name of the scope: Lifetime action to baseline.")
	cmd.Flags().BoolVar(&opts.export, "export", false, "Emit the ledger's current completions as a committable spec.baseline snippet.")
	return cmd
}

func runBaselineAdd(ctx context.Context, o *options, opts baselineOptions) error {
	c, _, err := o.newClient()
	if err != nil {
		return err
	}
	ns := o.namespace()
	key := client.ObjectKey{Namespace: ns, Name: opts.name}

	// Courtesy typo check: if the StageSet exists, the (stage, action) must name a
	// scope: Lifetime action. When it does not exist yet (ledger-before-StageSet
	// adoption), proceed — the reconciler validates continuously and holds an
	// unresolvable entry rather than dropping it.
	var ss stagesv1.StageSet
	switch ssErr := c.Get(ctx, key, &ss); {
	case ssErr == nil:
		if !isLifetimeActionInSpec(&ss, opts.stage, opts.action) {
			return fmt.Errorf("stage %q action %q is not a scope: Lifetime action in StageSet %q; only Lifetime actions are baselined", opts.stage, opts.action, opts.name)
		}
	case apierrors.IsNotFound(ssErr):
		fmt.Fprintf(o.streams.ErrOut, "note: StageSet %q not found yet; the baseline is validated when the StageSet is applied\n", opts.name)
	default:
		return ssErr
	}

	ref := stagesv1.LedgerRef{Stage: opts.stage, Action: opts.action}
	var ledger stagesv1.StageLedger
	switch lErr := c.Get(ctx, key, &ledger); {
	case apierrors.IsNotFound(lErr):
		ledger = stagesv1.StageLedger{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: opts.name}}
		ledger.Spec.Baseline = []stagesv1.LedgerRef{ref}
		if err := c.Create(ctx, &ledger, client.FieldOwner(reconcileFieldManager)); err != nil {
			return err
		}
		fmt.Fprintf(o.streams.Out, "Created StageLedger %s and baselined %s/%s\n", opts.name, opts.stage, opts.action)
		return nil
	case lErr != nil:
		return lErr
	}
	if hasBaselineRef(&ledger, ref) {
		fmt.Fprintf(o.streams.Out, "%s/%s is already in the baseline of StageLedger %s\n", opts.stage, opts.action, opts.name)
		return nil
	}
	ledger.Spec.Baseline = append(ledger.Spec.Baseline, ref)
	if err := c.Update(ctx, &ledger, client.FieldOwner(reconcileFieldManager)); err != nil {
		return err
	}
	fmt.Fprintf(o.streams.Out, "Baselined %s/%s in StageLedger %s\n", opts.stage, opts.action, opts.name)
	return nil
}

func runBaselineExport(ctx context.Context, o *options, opts baselineOptions) error {
	c, _, err := o.newClient()
	if err != nil {
		return err
	}
	ns := o.namespace()
	var ledger stagesv1.StageLedger
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: opts.name}, &ledger); err != nil {
		return err
	}

	out := stagesv1.StageLedger{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: opts.name}}
	out.SetGroupVersionKind(stagesv1.GroupVersion.WithKind("StageLedger"))
	for i := range ledger.Status.CompletedActions {
		comp := &ledger.Status.CompletedActions[i]
		out.Spec.Baseline = append(out.Spec.Baseline, stagesv1.LedgerRef{Stage: comp.Stage, Action: comp.Action})
	}
	if len(out.Spec.Baseline) == 0 {
		fmt.Fprintf(o.streams.ErrOut, "note: StageLedger %s has no completions to export\n", opts.name)
	}
	return encodeObject(o.streams.Out, &out, "yaml")
}

// --- reset-ledger ---

type resetLedgerOptions struct {
	name   string
	stage  string
	action string
	all    bool
}

func newResetLedgerCommand(o *options) *cobra.Command {
	opts := resetLedgerOptions{}
	cmd := &cobra.Command{
		Use:   "reset-ledger NAME [--stage STAGE --action ACTION | --all]",
		Short: "Forget a StageLedger completion so its scope: Lifetime action runs again",
		Long: "Remove a recorded completion from a StageLedger so its scope: Lifetime action is no longer " +
			"suppressed and runs on the next reconcile. Removes the matching spec.baseline assertion too, so a " +
			"Baselined completion is not immediately re-promoted. This re-runs a once-ever bootstrap: use it when " +
			"the underlying state was reset, or to repurpose a leftover ledger's name.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.name = args[0]
			if opts.all {
				if opts.stage != "" || opts.action != "" {
					return usageErr(fmt.Errorf("--all cannot be combined with --stage/--action"))
				}
			} else if opts.stage == "" || opts.action == "" {
				return usageErr(fmt.Errorf("specify --all, or both --stage and --action"))
			}
			return runtimeErr(runResetLedger(cmd.Context(), o, opts))
		},
	}
	cmd.Flags().StringVar(&opts.stage, "stage", "", "Stage of the completion to forget.")
	cmd.Flags().StringVar(&opts.action, "action", "", "Action of the completion to forget.")
	cmd.Flags().BoolVar(&opts.all, "all", false, "Forget every completion in the ledger.")
	return cmd
}

func runResetLedger(ctx context.Context, o *options, opts resetLedgerOptions) error {
	c, _, err := o.newClient()
	if err != nil {
		return err
	}
	ns := o.namespace()
	var ledger stagesv1.StageLedger
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: opts.name}, &ledger); err != nil {
		return err
	}

	match := func(stage, action string) bool {
		return opts.all || (stage == opts.stage && action == opts.action)
	}

	// Remove the spec.baseline assertion first, then the status completion — a
	// reconcile racing between the two writes re-promotes only from the reduced
	// spec, so a forgotten Baselined completion cannot reappear.
	baseline := ledger.Spec.Baseline[:0:0]
	specChanged := false
	for _, ref := range ledger.Spec.Baseline {
		if match(ref.Stage, ref.Action) {
			specChanged = true
			continue
		}
		baseline = append(baseline, ref)
	}
	if specChanged {
		ledger.Spec.Baseline = baseline
		if err := c.Update(ctx, &ledger, client.FieldOwner(reconcileFieldManager)); err != nil {
			return err
		}
	}

	kept := ledger.Status.CompletedActions[:0:0]
	removed := 0
	for _, comp := range ledger.Status.CompletedActions {
		if match(comp.Stage, comp.Action) {
			removed++
			continue
		}
		kept = append(kept, comp)
	}
	if removed > 0 {
		ledger.Status.CompletedActions = kept
		if err := c.Status().Update(ctx, &ledger, client.FieldOwner(reconcileFieldManager)); err != nil {
			return err
		}
	}
	fmt.Fprintf(o.streams.Out, "Reset %d completion(s) in StageLedger %s\n", removed, opts.name)
	return nil
}

// --- helpers ---

// isLifetimeActionInSpec reports whether (stage, action) names a scope: Lifetime
// pre/post action in the StageSet spec.
func isLifetimeActionInSpec(ss *stagesv1.StageSet, stage, action string) bool {
	st := specStage(ss, stage)
	if st == nil || st.Actions == nil {
		return false
	}
	for _, list := range [][]stagesv1.Action{st.Actions.Pre, st.Actions.Post} {
		for i := range list {
			if list[i].Name == action && list[i].EffectiveScope() == stagesv1.ScopeLifetime {
				return true
			}
		}
	}
	return false
}

func hasBaselineRef(ledger *stagesv1.StageLedger, ref stagesv1.LedgerRef) bool {
	return slices.Contains(ledger.Spec.Baseline, ref)
}
