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

// TestEvalBool_CostLimitBoundsExpensiveExpression pins that a deliberately
// expensive expression over a large input is aborted by the runtime cost limit
// rather than running unbounded — the CPU-pinning defence for remote-authored
// (sourced-ladder) wait expressions.
func TestEvalBool_CostLimitBoundsExpensiveExpression(t *testing.T) {
	t.Parallel()
	// A nested comprehension across a large list: cost grows multiplicatively.
	big := make([]any, 4000)
	for i := range big {
		big[i] = int64(i)
	}
	obj := map[string]any{"status": map[string]any{"xs": big}}

	p, err := Compile(`status.xs.all(a, status.xs.exists(b, a == b))`)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if _, err := p.EvalBool(obj); err == nil {
		t.Fatal("expected the cost limit to abort the expensive evaluation, got nil error")
	}
}
