// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package celeval

import "testing"

func TestEvalBool(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		expr    string
		obj     map[string]any
		want    bool
		evalErr bool
	}{
		{
			name: "int equality true",
			expr: "status.activeSessions == 0",
			obj:  map[string]any{"status": map[string]any{"activeSessions": int64(0)}},
			want: true,
		},
		{
			name: "int equality false",
			expr: "status.activeSessions == 0",
			obj:  map[string]any{"status": map[string]any{"activeSessions": int64(3)}},
			want: false,
		},
		{
			name:    "missing field is an eval error (treated as not-yet-satisfied)",
			expr:    "status.activeSessions == 0",
			obj:     map[string]any{"status": map[string]any{}},
			evalErr: true,
		},
		{
			name: "conditions filter (healthCheckExprs shape)",
			expr: `status.conditions.filter(c, c.type == 'Ready').exists(c, c.status == 'True')`,
			obj:  map[string]any{"status": map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}},
			want: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p, err := Compile(tc.expr)
			if err != nil {
				t.Fatalf("compile: %v", err)
			}
			got, err := p.EvalBool(tc.obj)
			if tc.evalErr {
				if err == nil {
					t.Fatal("expected an evaluation error")
				}
				return
			}
			if err != nil {
				t.Fatalf("eval: %v", err)
			}
			if got != tc.want {
				t.Fatalf("EvalBool = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCompileError(t *testing.T) {
	t.Parallel()
	if _, err := Compile("status.x +"); err == nil {
		t.Fatal("expected a compile error for a malformed expression")
	}
}

// TestCompile_RejectsNonBoolResult pins that an expression whose static result
// type is a concrete non-bool is rejected at compile time (it would otherwise
// only fail at eval, which pollers read as "not yet satisfied" → silent
// timeout). bool and dyn (bare field access on dyn-typed status) stay valid.
func TestCompile_RejectsNonBoolResult(t *testing.T) {
	t.Parallel()
	for _, expr := range []string{"1 + 2", `"ready"`, "size(spec.items)"} {
		if _, err := Compile(expr); err == nil {
			t.Errorf("Compile(%q) = nil, want a non-bool-result error", expr)
		}
	}
	for _, expr := range []string{"true", `status.phase == "Running"`, "status.ready", "status.replicas == status.readyReplicas"} {
		if _, err := Compile(expr); err != nil {
			t.Errorf("Compile(%q) = %v, want nil (bool/dyn is valid)", expr, err)
		}
	}
}

// TestEvalBool_CostLimitBoundsExpensiveExpression pins that a nested
// comprehension is aborted by the runtime cost limit rather than running
// unbounded — the CPU-pinning defence for remote-authored (sourced-ladder) wait
// expressions. A small input + tiny ceiling exercises the same code path the
// production maxEvalCost ceiling guards, without the multi-second eval a
// full-budget expression would take (especially under -race).
func TestEvalBool_CostLimitBoundsExpensiveExpression(t *testing.T) {
	t.Parallel()
	xs := make([]any, 50)
	for i := range xs {
		xs[i] = int64(i)
	}
	obj := map[string]any{"status": map[string]any{"xs": xs}}

	p, err := compileWithCost(`status.xs.all(a, status.xs.exists(b, a == b))`, 100)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if _, err := p.EvalBool(obj); err == nil {
		t.Fatal("expected the cost limit to abort the expensive evaluation, got nil error")
	}

	// A trivial expression stays well under the production ceiling.
	ok, err := Compile(`status.ready == true`)
	if err != nil {
		t.Fatalf("compile cheap: %v", err)
	}
	if _, err := ok.EvalBool(map[string]any{"status": map[string]any{"ready": true}}); err != nil {
		t.Fatalf("a cheap expression must not trip the cost limit: %v", err)
	}
}
