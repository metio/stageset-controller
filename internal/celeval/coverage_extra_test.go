// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package celeval

import "testing"

// A bare dyn-typed field access (`status.phase`) passes the compile-time
// bool/dyn gate but can resolve to a non-bool at runtime. EvalBool must surface
// that as an error — pollers read it as "not satisfied yet" — rather than
// panicking on the failed type assertion.
func TestEvalBool_DynExpressionResolvingToNonBoolIsAnError(t *testing.T) {
	t.Parallel()
	p, err := Compile("status.phase")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	got, err := p.EvalBool(map[string]any{"status": map[string]any{"phase": "Running"}})
	if err == nil {
		t.Fatalf("EvalBool of a string-valued dyn expression = %v, want a non-bool error", got)
	}
}

// A dyn field that does resolve to a bool at runtime evaluates cleanly through
// the same code path, both true and false.
func TestEvalBool_DynExpressionResolvingToBool(t *testing.T) {
	t.Parallel()
	p, err := Compile("status.ready")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	for _, tc := range []struct {
		name string
		val  any
		want bool
	}{
		{name: "true", val: true, want: true},
		{name: "false", val: false, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := p.EvalBool(map[string]any{"status": map[string]any{"ready": tc.val}})
			if err != nil {
				t.Fatalf("eval: %v", err)
			}
			if got != tc.want {
				t.Fatalf("EvalBool = %v, want %v", got, tc.want)
			}
		})
	}
}

// Top-level apiVersion and kind are string-typed variables; an expression over
// them compiles and evaluates against the values lifted off the object.
func TestEvalBool_OverApiVersionAndKind(t *testing.T) {
	t.Parallel()
	p, err := Compile(`apiVersion == "apps/v1" && kind == "Deployment"`)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	got, err := p.EvalBool(map[string]any{"apiVersion": "apps/v1", "kind": "Deployment"})
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	if !got {
		t.Fatal("EvalBool over apiVersion/kind = false, want true")
	}
}

// A wholly absent top-level field is presented as an empty map, so an expression
// that dereferences it returns an evaluation error rather than panicking.
func TestEvalBool_AbsentTopLevelFieldIsEmptyMap(t *testing.T) {
	t.Parallel()
	p, err := Compile("status.ready")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if _, err := p.EvalBool(map[string]any{}); err == nil {
		t.Fatal("EvalBool over an object with no status = nil error, want an eval error")
	}
}

// compileWithCost surfaces the underlying CEL compile diagnostic for a malformed
// expression regardless of the cost ceiling.
func TestCompileWithCost_MalformedExpression(t *testing.T) {
	t.Parallel()
	if _, err := compileWithCost("status.x ==", maxEvalCost); err == nil {
		t.Fatal("compileWithCost of a malformed expression = nil, want a compile error")
	}
}
