// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

// Command flaggen introspects the stageset-controller's own CLI FlagSet and
// emits a JSON array describing every flag (name, default, usage, group) for the
// docs site to render. It builds the same FlagSet cmd/main uses via the shared
// cliflags.Register seam, so the generated reference can never drift from the
// runtime contract. The controller-runtime zap flags are not registered here, so
// they stay prose in the configuration reference.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"

	"github.com/metio/stageset-controller/internal/cliflags"
)

// flagDoc is one row of the generated flags table. Field order is the column
// order the Hugo shortcode renders. Go's stdlib flag exposes no type, so there
// is no type column.
type flagDoc struct {
	Name    string `json:"name"`
	Default string `json:"default"`
	Usage   string `json:"usage"`
	Group   string `json:"group"`
}

func main() {
	out := flag.String("o", "docs/data/flags.json", "output path for the generated flags JSON")
	flag.Parse()

	if err := generate(*out); err != nil {
		fmt.Fprintln(os.Stderr, "flaggen:", err)
		os.Exit(1)
	}
}

func generate(outPath string) error {
	fs := flag.NewFlagSet("stageset-controller", flag.ContinueOnError)
	cliflags.Register(fs)

	// Group → index for stable, documentation-ordered output. Flags whose group
	// is unset or unknown sort to the end under their literal group string, so a
	// missing annotation surfaces rather than silently vanishing.
	order := map[string]int{}
	for i, g := range cliflags.Groups() {
		order[g] = i
	}

	var docs []flagDoc
	fs.VisitAll(func(fl *flag.Flag) {
		docs = append(docs, flagDoc{
			Name:    fl.Name,
			Default: fl.DefValue,
			Usage:   fl.Usage,
			Group:   cliflags.GroupOf(fl.Name),
		})
	})

	sort.SliceStable(docs, func(i, j int) bool {
		gi, iok := order[docs[i].Group]
		gj, jok := order[docs[j].Group]
		if iok != jok {
			// Known groups sort before unknown ones.
			return iok
		}
		if iok && gi != gj {
			return gi < gj
		}
		if docs[i].Group != docs[j].Group {
			return docs[i].Group < docs[j].Group
		}
		return docs[i].Name < docs[j].Name
	})

	body, err := json.MarshalIndent(docs, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	body = append(body, '\n')
	// #nosec G306 -- generated documentation data, not a secret.
	if err := os.WriteFile(outPath, body, 0o644); err != nil {
		return fmt.Errorf("write %q: %w", outPath, err)
	}
	return nil
}
