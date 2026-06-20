// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package cli

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/Masterminds/semver/v3"
	"github.com/spf13/cobra"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/migrations"
)

func newLintMigrationsCommand(o *options) *cobra.Command {
	var from, to string
	cmd := &cobra.Command{
		Use:   "lint-migrations PATH",
		Short: "Validate a migration ladder file or directory before publishing",
		Long: "Parse and validate a migration ladder — a single file, or a directory of .yaml/.yml/.json files — with " +
			"the exact checks the controller runs at admission and at sourced-artifact fetch time: strict parsing " +
			"(a misspelled field is rejected), exactly one verb per action, unique migration and action names, and " +
			"valid semver in `to` plus a valid constraint in `from`. Prints the parsed ladder on success; on the first " +
			"problem it prints the problem and exits 1, so it can gate CI before the ladder is published to a source.\n\n" +
			"With --from and --to it also simulates a version transition and reports which migrations would fire and " +
			"which are excluded and why — the same selection the controller runs — so the otherwise-invisible " +
			"from/to math is visible before it hits a cluster.",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return runLintMigrations(o, args[0], from, to)
		},
	}
	cmd.Flags().StringVar(&from, "from", "", "Simulate a transition from this (current) version; reports which migrations fire. Requires --to.")
	cmd.Flags().StringVar(&to, "to", "", "Simulate a transition to this (desired) version. Requires --from.")
	return cmd
}

func runLintMigrations(o *options, path, from, to string) error {
	if (from == "") != (to == "") {
		return runtimeErr(fmt.Errorf("--from and --to must be given together"))
	}
	files, err := readLadderFiles(path)
	if err != nil {
		return runtimeErr(err)
	}

	w := o.streams.Out
	ladder, err := migrations.ParseLadder(files)
	if err == nil {
		err = migrations.ValidateLadder(ladder)
	}
	if err != nil {
		fmt.Fprintf(w, "✗ %s\n", err)
		return &exitErr{code: exitDiff} // problems found; message already printed
	}

	fmt.Fprintf(w, "✓ %d migration(s) valid\n", len(ladder))
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	for i := range ladder {
		m := &ladder[i]
		boundary := "→ " + m.To
		if m.From != "" {
			boundary = m.From + " → " + m.To
		}
		anchor := m.Stage
		if anchor == "" {
			anchor = "(first stage)"
		}
		verbs := make([]string, len(m.Actions))
		for j := range m.Actions {
			verbs[j] = m.Actions[j].Verb()
		}
		fmt.Fprintf(tw, "  %s\t%s\tbefore %s\t%s\n", m.Name, boundary, anchor, strings.Join(verbs, ", "))
	}
	_ = tw.Flush()

	if from != "" {
		return reportTransition(o, ladder, from, to)
	}
	return nil
}

// reportTransition prints which migrations a from→to transition fires and why
// the rest are excluded, using the controller's own selection logic.
func reportTransition(o *options, ladder []stagesv1.Migration, from, to string) error {
	fromV, err := semver.NewVersion(from)
	if err != nil {
		return runtimeErr(fmt.Errorf("--from %q is not a semver: %w", from, err))
	}
	toV, err := semver.NewVersion(to)
	if err != nil {
		return runtimeErr(fmt.Errorf("--to %q is not a semver: %w", to, err))
	}
	outcomes, err := migrations.Explain(ladder, fromV, toV)
	if err != nil {
		return runtimeErr(err)
	}

	w := o.streams.Out
	fmt.Fprintf(w, "\ntransition %s → %s:\n", from, to)
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	for _, oc := range outcomes {
		if oc.Fires {
			verbs := make([]string, len(oc.Migration.Actions))
			for j := range oc.Migration.Actions {
				verbs[j] = oc.Migration.Actions[j].Verb()
			}
			fmt.Fprintf(tw, "  %s\tFIRES\t%s\n", oc.Migration.Name, strings.Join(verbs, ", "))
		} else {
			fmt.Fprintf(tw, "  %s\texcluded\t%s\n", oc.Migration.Name, oc.Reason)
		}
	}
	_ = tw.Flush()
	return nil
}

// readLadderFiles reads PATH into a path→content map: a single file keyed by its
// base name, or every file under a directory keyed by its path relative to it.
// ParseLadder filters to the migration file extensions, so non-ladder files in a
// directory are harmless.
func readLadderFiles(path string) (map[string]string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	files := map[string]string{}
	if !info.IsDir() {
		// #nosec G304 -- path is the operator-supplied lint target; reading the file the user named is the command's purpose.
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil, rerr
		}
		files[filepath.Base(path)] = string(b)
		return files, nil
	}
	walkErr := filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		// #nosec G304 G122 -- p is under the operator-supplied directory being linted; a local CLI reading the user's own ladder files, not a server-resolved path.
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		rel, rerr := filepath.Rel(path, p)
		if rerr != nil {
			rel = p
		}
		files[rel] = string(b)
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	return files, nil
}
