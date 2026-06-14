// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

// Package celeval compiles and evaluates boolean CEL expressions over a
// Kubernetes object's unstructured content. Expressions reference the object's
// top-level fields directly (apiVersion, kind, metadata, spec, status) — the
// same shape kustomize-controller's healthCheckExprs use — so users copy
// expressions verbatim.
package celeval

import (
	"fmt"

	"github.com/google/cel-go/cel"
)

// Program is a compiled boolean CEL expression.
type Program struct {
	program cel.Program
}

// Compile builds a Program from a CEL expression. The expression must evaluate
// to a bool.
func Compile(expr string) (*Program, error) {
	env, err := cel.NewEnv(
		cel.Variable("apiVersion", cel.StringType),
		cel.Variable("kind", cel.StringType),
		cel.Variable("metadata", cel.DynType),
		cel.Variable("spec", cel.DynType),
		cel.Variable("status", cel.DynType),
	)
	if err != nil {
		return nil, fmt.Errorf("cel env: %w", err)
	}
	ast, iss := env.Compile(expr)
	if iss != nil && iss.Err() != nil {
		return nil, fmt.Errorf("compile CEL %q: %w", expr, iss.Err())
	}
	program, err := env.Program(ast)
	if err != nil {
		return nil, fmt.Errorf("build CEL program: %w", err)
	}
	return &Program{program: program}, nil
}

// EvalBool evaluates the program against an object's unstructured content. A
// missing top-level field is presented as an empty map so an expression that
// dereferences, say, a not-yet-populated status returns an evaluation error
// (which pollers treat as "not satisfied yet") rather than panicking.
func (p *Program) EvalBool(obj map[string]any) (bool, error) {
	out, _, err := p.program.Eval(map[string]any{
		"apiVersion": asString(obj["apiVersion"]),
		"kind":       asString(obj["kind"]),
		"metadata":   asMap(obj["metadata"]),
		"spec":       asMap(obj["spec"]),
		"status":     asMap(obj["status"]),
	})
	if err != nil {
		return false, err
	}
	b, ok := out.Value().(bool)
	if !ok {
		return false, fmt.Errorf("CEL expression returned %T, want bool", out.Value())
	}
	return b, nil
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func asMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}
