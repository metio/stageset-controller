// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package diffrender

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

// ActionPreview is one stage action a rollout would run, for the diff's actions
// section. Only actions that will actually fire (not already satisfied by the
// idempotency ledger) are expected here.
type ActionPreview struct {
	Stage  string
	Phase  string // pre | post | onFailure
	Name   string
	Type   string // patch | http | wait | job | delete | apply
	Detail string
}

// MigrationPreview is one migration the next run will execute, for the diff's
// migrations section. The list is the controller's own pending set, so it
// appears only when migrations will actually run.
type MigrationPreview struct {
	Name    string
	To      string
	From    string
	Stage   string
	Actions []string // action verbs in run order
}

// WriteActions renders the actions a rollout would run, grouped by stage and
// phase. Nothing is written when there are no actions, so a clean diff stays
// quiet.
func WriteActions(w io.Writer, actions []ActionPreview, color bool) {
	if len(actions) == 0 {
		return
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, headingText("Actions to run:", color))

	byStage := map[string][]ActionPreview{}
	var order []string
	for _, a := range actions {
		if _, seen := byStage[a.Stage]; !seen {
			order = append(order, a.Stage)
		}
		byStage[a.Stage] = append(byStage[a.Stage], a)
	}

	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	for _, stage := range order {
		fmt.Fprintf(tw, "  %s:\n", stage)
		for _, a := range byStage[stage] {
			detail := a.Type
			if a.Detail != "" {
				detail += " " + a.Detail
			}
			fmt.Fprintf(tw, "    %s\t%s\t%s\n", a.Phase, a.Name, detail)
		}
	}
	_ = tw.Flush()
}

// WriteMigrations renders the migrations the next run will execute. Nothing is
// written when none are pending.
func WriteMigrations(w io.Writer, migrations []MigrationPreview, color bool) {
	if len(migrations) == 0 {
		return
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, headingText("Migrations to run:", color))

	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	for _, m := range migrations {
		boundary := "→ " + m.To
		if m.From != "" {
			boundary = m.From + " → " + m.To
		}
		fmt.Fprintf(tw, "  %s\t%s\tbefore stage %s\t%s\n",
			m.Name, boundary, m.Stage, actionsSummary(m.Actions))
	}
	_ = tw.Flush()
}

func headingText(s string, color bool) string {
	if !color {
		return s
	}
	return ansiCyan + s + ansiReset
}

// actionsSummary renders a migration's action verbs as a count plus the verbs,
// e.g. "2 actions: delete, apply", so the preview shows what destructive work
// runs, not just how many steps.
func actionsSummary(verbs []string) string {
	if len(verbs) == 0 {
		return "no actions"
	}
	noun := "actions"
	if len(verbs) == 1 {
		noun = "action"
	}
	return fmt.Sprintf("%d %s: %s", len(verbs), noun, strings.Join(verbs, ", "))
}
