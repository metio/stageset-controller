// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package diffrender

import (
	"fmt"
	"io"
	"strings"

	"github.com/pmezard/go-difflib/difflib"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// ChangeKind is the predicted change for one object in a diff.
type ChangeKind string

const (
	ChangeCreate    ChangeKind = "create"
	ChangeConfigure ChangeKind = "configure"
	ChangeDelete    ChangeKind = "delete"
	ChangeUnchanged ChangeKind = "unchanged"
	ChangeSkip      ChangeKind = "skip"
)

// Change is one object's predicted change. Before is the live object (nil for a
// create); After is the object as it would be after apply (nil for a delete).
type Change struct {
	Stage     string
	Kind      ChangeKind
	GVK       schema.GroupVersionKind
	Namespace string
	Name      string
	Before    *unstructured.Unstructured
	After     *unstructured.Unstructured
}

// Summary counts predicted changes by kind for the trailing summary line and
// the diff(1) exit-code decision.
type Summary struct {
	Create    int
	Configure int
	Delete    int
	Unchanged int
	Skip      int
}

// Changed reports whether anything would change (create/configure/delete),
// which maps to the diff(1) "differences found" exit code.
func (s Summary) Changed() bool {
	return s.Create+s.Configure+s.Delete > 0
}

// RenderOptions controls diff presentation.
type RenderOptions struct {
	ShowUnchanged bool
	Color         bool
	Masker        *SecretMasker
}

// RenderDiff writes a per-object unified diff grouped in input order and
// returns the change summary. Server-noise is stripped and Secret values masked
// on both sides before diffing, so output is stable and leak-free.
func RenderDiff(w io.Writer, changes []Change, opts RenderOptions) (Summary, error) {
	masker := opts.Masker
	if masker == nil {
		masker = NewSecretMasker(false)
	}
	var sum Summary
	for _, ch := range changes {
		switch ch.Kind {
		case ChangeCreate:
			sum.Create++
		case ChangeConfigure:
			sum.Configure++
		case ChangeDelete:
			sum.Delete++
		case ChangeUnchanged:
			sum.Unchanged++
			if !opts.ShowUnchanged {
				continue
			}
		case ChangeSkip:
			sum.Skip++
			if !opts.ShowUnchanged {
				continue
			}
		}

		before, err := renderSide(ch.Before, masker)
		if err != nil {
			return sum, err
		}
		after, err := renderSide(ch.After, masker)
		if err != nil {
			return sum, err
		}

		fmt.Fprintln(w, header(ch, opts.Color))
		body, err := unifiedBody(before, after)
		if err != nil {
			return sum, err
		}
		if body != "" {
			fmt.Fprint(w, colorize(body, opts.Color))
		}
	}
	return sum, nil
}

// WriteSummary prints the trailing change-summary line. It is separate from
// RenderDiff so the diff command can place it after the actions and migrations
// sections, keeping it the last line of output.
func WriteSummary(w io.Writer, sum Summary) {
	fmt.Fprintln(w, summaryLine(sum))
}

// renderSide strips noise, masks Secrets, and YAML-encodes one side of a diff.
// A nil object (create's before, delete's after) renders as empty.
func renderSide(obj *unstructured.Unstructured, masker *SecretMasker) (string, error) {
	if obj == nil {
		return "", nil
	}
	c := obj.DeepCopy()
	StripNoise(c)
	masker.Mask(c)
	out, err := ToYAML(c)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func header(ch Change, color bool) string {
	kind := ch.GVK.Kind
	id := kind + "/" + ch.Name
	if ch.Namespace != "" {
		id += " [" + ch.Namespace + "]"
	}
	label := fmt.Sprintf("%s %s", ch.Kind, id)
	if ch.Stage != "" {
		label += fmt.Sprintf("  (stage: %s)", ch.Stage)
	}
	if !color {
		return label
	}
	return colorFor(ch.Kind) + label + ansiReset
}

func unifiedBody(before, after string) (string, error) {
	if before == after {
		return "", nil
	}
	return difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
		A:        difflib.SplitLines(before),
		B:        difflib.SplitLines(after),
		FromFile: "live",
		ToFile:   "merged",
		Context:  3,
	})
}

func summaryLine(s Summary) string {
	var parts []string
	if s.Create > 0 {
		parts = append(parts, fmt.Sprintf("%d to create", s.Create))
	}
	if s.Configure > 0 {
		parts = append(parts, fmt.Sprintf("%d to configure", s.Configure))
	}
	if s.Delete > 0 {
		parts = append(parts, fmt.Sprintf("%d to delete", s.Delete))
	}
	if s.Unchanged > 0 {
		parts = append(parts, fmt.Sprintf("%d unchanged", s.Unchanged))
	}
	if s.Skip > 0 {
		parts = append(parts, fmt.Sprintf("%d skipped", s.Skip))
	}
	if len(parts) == 0 {
		return "Summary: no changes"
	}
	return "Summary: " + strings.Join(parts, ", ")
}

// --- color ---

const (
	ansiReset = "\x1b[0m"
	ansiRed   = "\x1b[31m"
	ansiGreen = "\x1b[32m"
	ansiCyan  = "\x1b[36m"
)

func colorFor(k ChangeKind) string {
	switch k {
	case ChangeCreate:
		return ansiGreen
	case ChangeDelete:
		return ansiRed
	case ChangeConfigure:
		return ansiCyan
	default:
		return ""
	}
}

// colorize tints unified-diff gutters: additions green, removals red, hunk
// headers cyan. Color is additive over the +/- prefix so piped output stays
// readable when disabled.
func colorize(body string, color bool) string {
	if !color {
		return body
	}
	lines := strings.SplitAfter(body, "\n")
	var b strings.Builder
	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "---"):
			b.WriteString(line)
		case strings.HasPrefix(line, "+"):
			b.WriteString(ansiGreen + strings.TrimSuffix(line, "\n") + ansiReset)
			if strings.HasSuffix(line, "\n") {
				b.WriteString("\n")
			}
		case strings.HasPrefix(line, "-"):
			b.WriteString(ansiRed + strings.TrimSuffix(line, "\n") + ansiReset)
			if strings.HasSuffix(line, "\n") {
				b.WriteString("\n")
			}
		case strings.HasPrefix(line, "@@"):
			b.WriteString(ansiCyan + strings.TrimSuffix(line, "\n") + ansiReset)
			if strings.HasSuffix(line, "\n") {
				b.WriteString("\n")
			}
		default:
			b.WriteString(line)
		}
	}
	return b.String()
}
