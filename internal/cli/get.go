// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

// conditionReady is the StageSet's summary condition type — the Flux "Ready"
// convention the controller writes and notification-controller keys on.
const conditionReady = "Ready"

type getOptions struct {
	name          string
	allNamespaces bool
	output        string
}

func newGetCommand(o *options) *cobra.Command {
	opts := getOptions{}
	cmd := &cobra.Command{
		Use:   "get [NAME]",
		Short: "Print human-readable StageSet status",
		Long: "Print StageSet status that `kubectl get` cannot assemble: Ready reason, per-stage " +
			"phase and applied revision, held updates and the next window, deployed version and pending migrations.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				opts.name = args[0]
			}
			return runtimeErr(runGet(cmd.Context(), o, opts))
		},
	}
	cmd.Flags().BoolVarP(&opts.allNamespaces, "all-namespaces", "A", false, "List StageSets across all namespaces.")
	cmd.Flags().StringVarP(&opts.output, "output", "o", "", "Output format: empty for a human table, or yaml/json.")
	return cmd
}

func runGet(ctx context.Context, o *options, opts getOptions) error {
	if opts.output != "" && opts.output != "yaml" && opts.output != "json" {
		return fmt.Errorf("invalid --output %q: want yaml or json", opts.output)
	}
	c, _, err := o.newClient()
	if err != nil {
		return err
	}

	if opts.name != "" {
		var ss stagesv1.StageSet
		key := client.ObjectKey{Namespace: o.namespace(), Name: opts.name}
		if err := c.Get(ctx, key, &ss); err != nil {
			return err
		}
		switch opts.output {
		case "yaml", "json":
			ss.SetGroupVersionKind(stagesv1.GroupVersion.WithKind("StageSet"))
			return encodeObject(o.streams.Out, &ss, opts.output)
		default:
			writeDetail(o.streams.Out, &ss)
			return nil
		}
	}

	var list stagesv1.StageSetList
	var listOpts []client.ListOption
	if !opts.allNamespaces {
		listOpts = append(listOpts, client.InNamespace(o.namespace()))
	}
	if err := c.List(ctx, &list, listOpts...); err != nil {
		return err
	}
	sort.Slice(list.Items, func(i, j int) bool {
		if list.Items[i].Namespace != list.Items[j].Namespace {
			return list.Items[i].Namespace < list.Items[j].Namespace
		}
		return list.Items[i].Name < list.Items[j].Name
	})

	switch opts.output {
	case "yaml", "json":
		list.SetGroupVersionKind(stagesv1.GroupVersion.WithKind("StageSetList"))
		return encodeObject(o.streams.Out, &list, opts.output)
	default:
		writeTable(o.streams.Out, list.Items, opts.allNamespaces)
		return nil
	}
}

func encodeObject(w io.Writer, obj any, format string) error {
	if format == "json" {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(obj)
	}
	out, err := yaml.Marshal(obj)
	if err != nil {
		return err
	}
	_, err = w.Write(out)
	return err
}

// writeTable renders one row per StageSet: readiness, stage progress, deployed
// version, and whether an update is held.
func writeTable(w io.Writer, items []stagesv1.StageSet, withNamespace bool) {
	if len(items) == 0 {
		fmt.Fprintln(w, "No StageSets found.")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	if withNamespace {
		fmt.Fprint(tw, "NAMESPACE\t")
	}
	fmt.Fprintln(tw, "NAME\tREADY\tREASON\tSTAGES\tVERSION\tPENDING")
	for i := range items {
		ss := &items[i]
		ready, reason := readyState(ss)
		if withNamespace {
			fmt.Fprintf(tw, "%s\t", ss.Namespace)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			ss.Name, ready, reason, stageProgress(ss), versionOrDash(ss.Status.Version), pendingSummary(ss))
	}
	_ = tw.Flush()
}

// writeDetail renders the full status of a single StageSet.
func writeDetail(w io.Writer, ss *stagesv1.StageSet) {
	fmt.Fprintf(w, "Name:       %s\n", ss.Name)
	fmt.Fprintf(w, "Namespace:  %s\n", ss.Namespace)
	if ss.Spec.Suspend {
		fmt.Fprintln(w, "Suspended:  true")
	}

	cond := apimeta.FindStatusCondition(ss.Status.Conditions, conditionReady)
	if cond != nil {
		fmt.Fprintf(w, "Ready:      %s (%s)\n", cond.Status, cond.Reason)
		if cond.Message != "" {
			fmt.Fprintf(w, "Message:    %s\n", cond.Message)
		}
	} else {
		fmt.Fprintln(w, "Ready:      Unknown")
	}

	fmt.Fprintf(w, "Version:    %s\n", versionOrDash(ss.Status.Version))
	if len(ss.Status.PendingMigrations) > 0 {
		fmt.Fprintf(w, "Pending migrations: %s\n", strings.Join(ss.Status.PendingMigrations, ", "))
	}
	if rev := ss.Status.GetLastHandledReconcileRequest(); rev != "" {
		fmt.Fprintf(w, "Last handled reconcile: %s\n", rev)
	}

	if pu := ss.Status.PendingUpdate; pu != nil {
		fmt.Fprintln(w, "Pending update:")
		if pu.NextWindowOpens != nil {
			fmt.Fprintf(w, "  Next window opens: %s\n", pu.NextWindowOpens.Format(metav1.RFC3339Micro))
		}
		for _, k := range sortedKeys(pu.Revisions) {
			fmt.Fprintf(w, "  Held: %s -> %s\n", k, pu.Revisions[k])
		}
	}

	if len(ss.Status.Stages) > 0 {
		fmt.Fprintln(w, "Stages:")
		tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "  NAME\tPHASE\tREVISION\tENTRIES")
		for _, st := range ss.Status.Stages {
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%d\n",
				st.Name, phaseOrDash(st.Phase), revisionOrDash(st.AppliedRevision), st.EntriesCount)
		}
		_ = tw.Flush()
	}
}

func readyState(ss *stagesv1.StageSet) (string, string) {
	cond := apimeta.FindStatusCondition(ss.Status.Conditions, conditionReady)
	if cond == nil {
		return "Unknown", "-"
	}
	return string(cond.Status), cond.Reason
}

func stageProgress(ss *stagesv1.StageSet) string {
	total := len(ss.Spec.Stages)
	ready := 0
	for _, st := range ss.Status.Stages {
		if st.Phase == stagesv1.StageReady {
			ready++
		}
	}
	return fmt.Sprintf("%d/%d", ready, total)
}

func pendingSummary(ss *stagesv1.StageSet) string {
	pu := ss.Status.PendingUpdate
	if pu == nil {
		return "-"
	}
	if pu.NextWindowOpens != nil {
		return "held until " + pu.NextWindowOpens.Format(metav1.RFC3339Micro)
	}
	return "held"
}

func versionOrDash(v string) string {
	if v == "" {
		return "-"
	}
	return v
}

func revisionOrDash(v string) string {
	if v == "" {
		return "-"
	}
	return v
}

func phaseOrDash(p stagesv1.StagePhase) string {
	if p == "" {
		return "-"
	}
	return string(p)
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
