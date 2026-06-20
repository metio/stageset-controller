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

	"github.com/spf13/cobra"

	"github.com/metio/stageset-controller/internal/migrations"
)

func newLintMigrationsCommand(o *options) *cobra.Command {
	return &cobra.Command{
		Use:   "lint-migrations PATH",
		Short: "Validate a migration ladder file or directory before publishing",
		Long: "Parse and validate a migration ladder — a single file, or a directory of .yaml/.yml/.json files — with " +
			"the exact checks the controller runs at admission and at sourced-artifact fetch time: strict parsing " +
			"(a misspelled field is rejected), exactly one verb per action, unique migration and action names, and " +
			"valid semver in `to` plus a valid constraint in `from`. Prints the parsed ladder on success; on the first " +
			"problem it prints the problem and exits 1, so it can gate CI before the ladder is published to a source.",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return runLintMigrations(o, args[0])
		},
	}
}

func runLintMigrations(o *options, path string) error {
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
