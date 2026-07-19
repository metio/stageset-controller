// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package cli

import (
	"context"
	"fmt"
	"io"
	"sort"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

type fleetOptions struct {
	name   string
	output string
}

func newFleetCommand(o *options) *cobra.Command {
	opts := fleetOptions{}
	cmd := &cobra.Command{
		Use:   "fleet NAME",
		Short: "Show a FleetRollout's wave-by-wave progress",
		Long: "Print a FleetRollout's rollout progress the way `plan` prints a StageSet's: overall phase and target " +
			"version, then each wave with how many members have reached the version and gone Ready, its soak and health " +
			"state, and — per member — whether it is at the target, still held awaiting its wave, or regressed. Read-only.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.name = args[0]
			if opts.output != "" && opts.output != "yaml" && opts.output != "json" {
				return usageErr(fmt.Errorf("invalid --output %q: want yaml or json", opts.output))
			}
			return runtimeErr(runFleet(cmd.Context(), o, opts))
		},
	}
	cmd.Flags().StringVarP(&opts.output, "output", "o", "", "Output format: empty for a human view, or yaml/json.")
	return cmd
}

func runFleet(ctx context.Context, o *options, opts fleetOptions) error {
	c, _, err := o.newClient()
	if err != nil {
		return err
	}
	var fr stagesv1.FleetRollout
	if err := c.Get(ctx, types.NamespacedName{Name: opts.name}, &fr); err != nil {
		return err
	}
	switch opts.output {
	case "yaml", "json":
		fr.SetGroupVersionKind(stagesv1.GroupVersion.WithKind("FleetRollout"))
		return encodeObject(o.streams.Out, &fr, opts.output)
	}
	members, err := fleetMembers(ctx, c, &fr)
	if err != nil {
		return err
	}
	writeFleetView(o.streams.Out, &fr, members)
	return nil
}

// fleetMembers resolves the StageSets the rollout selects (spec.selector bounded by
// spec.namespaceSelector), mirroring the controller so the view matches what the
// reconciler acts on.
func fleetMembers(ctx context.Context, c client.Client, fr *stagesv1.FleetRollout) ([]stagesv1.StageSet, error) {
	sel, err := metav1.LabelSelectorAsSelector(&fr.Spec.Selector)
	if err != nil {
		return nil, err
	}
	var list stagesv1.StageSetList
	if err := c.List(ctx, &list, client.MatchingLabelsSelector{Selector: sel}); err != nil {
		return nil, err
	}
	if fr.Spec.NamespaceSelector == nil {
		return list.Items, nil
	}
	nsSel, err := metav1.LabelSelectorAsSelector(fr.Spec.NamespaceSelector)
	if err != nil {
		return nil, err
	}
	var nsList corev1.NamespaceList
	if err := c.List(ctx, &nsList); err != nil {
		return nil, err
	}
	inScope := map[string]bool{}
	for i := range nsList.Items {
		if nsSel.Matches(labels.Set(nsList.Items[i].Labels)) {
			inScope[nsList.Items[i].Name] = true
		}
	}
	out := list.Items[:0]
	for i := range list.Items {
		if inScope[list.Items[i].Namespace] {
			out = append(out, list.Items[i])
		}
	}
	return out, nil
}

func writeFleetView(out io.Writer, fr *stagesv1.FleetRollout, members []stagesv1.StageSet) {
	target := fr.Spec.TargetVersion
	fmt.Fprintf(out, "FleetRollout %s  →  %s\n", fr.Name, fleetDisplayVersion(fr, members))
	phase := string(fr.Status.Phase)
	if phase == "" {
		phase = "Pending"
	}
	line := "  phase: " + phase
	if fr.Status.CurrentWave != "" {
		line += "   wave: " + fr.Status.CurrentWave
	}
	fmt.Fprintln(out, line)
	if cond := apimeta.FindStatusCondition(fr.Status.Conditions, conditionReady); cond != nil && cond.Status != metav1.ConditionTrue {
		fmt.Fprintf(out, "  %s: %s\n", cond.Reason, cond.Message)
	}

	waveStatus := map[string]stagesv1.FleetWaveStatus{}
	for _, ws := range fr.Status.Waves {
		waveStatus[ws.Name] = ws
	}
	for i := range fr.Spec.Waves {
		wave := &fr.Spec.Waves[i]
		ws := waveStatus[wave.Name]
		label := "adopted"
		if target != "" {
			label = "at " + target
		}
		fmt.Fprintf(out, "  wave %s   %d/%d %s, %d ready%s\n",
			wave.Name, ws.AtTarget, ws.Total, label, ws.Ready, waveState(ws))
		sel, err := metav1.LabelSelectorAsSelector(&wave.Selector)
		if err != nil {
			continue
		}
		var names []string
		byName := map[string]stagesv1.StageSet{}
		for j := range members {
			if sel.Matches(labels.Set(members[j].Labels)) {
				key := members[j].Namespace + "/" + members[j].Name
				names = append(names, key)
				byName[key] = members[j]
			}
		}
		sort.Strings(names)
		for _, key := range names {
			m := byName[key]
			fmt.Fprintf(out, "    %s %-28s %s\n", memberMark(fr, &m), key, memberState(fr, &m))
		}
	}
}

// waveState renders a settled wave's soak/health suffix.
func waveState(ws stagesv1.FleetWaveStatus) string {
	if !ws.Settled {
		return ""
	}
	if ws.Health == "Failing" {
		return "   health: Failing"
	}
	suffix := "   settled"
	if ws.Health != "" {
		suffix += ", health: " + ws.Health
	}
	return suffix
}

// memberAdopted reports whether a member has reached its intended version: the
// pinned spec.targetVersion when set, otherwise no advance awaiting approval (the
// derived default).
func memberAdopted(fr *stagesv1.FleetRollout, m *stagesv1.StageSet) bool {
	if fr.Spec.TargetVersion != "" {
		return m.Status.Version == fr.Spec.TargetVersion
	}
	return m.Status.PendingVersion == ""
}

// memberMark is a one-glyph status for a member: adopted, regressed, or held.
func memberMark(fr *stagesv1.FleetRollout, m *stagesv1.StageSet) string {
	switch {
	case memberAdopted(fr, m) && stageSetReady(m):
		return "✓"
	case memberAdopted(fr, m):
		return "⚠"
	default:
		return "…"
	}
}

func memberState(fr *stagesv1.FleetRollout, m *stagesv1.StageSet) string {
	switch {
	case memberAdopted(fr, m) && stageSetReady(m):
		return orNoneVersion(m.Status.Version) + "  Ready"
	case memberAdopted(fr, m):
		return orNoneVersion(m.Status.Version) + "  not Ready (regressed)"
	default:
		awaiting := m.Status.PendingVersion
		if fr.Spec.TargetVersion != "" {
			awaiting = fr.Spec.TargetVersion
		}
		return orNoneVersion(m.Status.Version) + "  held → awaiting " + orNoneVersion(awaiting)
	}
}

func orNoneVersion(v string) string {
	if v == "" {
		return "(none)"
	}
	return v
}

// fleetDisplayVersion is the version shown in the header: the pinned target when
// set, otherwise the single distinct version members are advancing to (or a
// summary).
func fleetDisplayVersion(fr *stagesv1.FleetRollout, members []stagesv1.StageSet) string {
	if fr.Spec.TargetVersion != "" {
		return "version " + fr.Spec.TargetVersion
	}
	pending := map[string]bool{}
	for i := range members {
		if v := members[i].Status.PendingVersion; v != "" {
			pending[v] = true
		}
	}
	switch len(pending) {
	case 0:
		return "the current version"
	case 1:
		for v := range pending {
			return "version " + v
		}
	}
	return "mixed versions"
}

func stageSetReady(m *stagesv1.StageSet) bool {
	c := apimeta.FindStatusCondition(m.Status.Conditions, conditionReady)
	return c != nil && c.Status == metav1.ConditionTrue && m.Status.ObservedGeneration == m.Generation
}
