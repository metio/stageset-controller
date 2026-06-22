// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package apply_test

import (
	"context"
	"testing"
	"time"

	"github.com/fluxcd/cli-utils/pkg/object"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/apply"
)

// The exported NewForceToken seam mints a fresh, non-empty token on each call so
// two consecutive applies never share a force selector value.
func TestNewForceToken_FreshAndNonEmpty(t *testing.T) {
	t.Parallel()
	seen := map[string]struct{}{}
	for range 50 {
		tok := apply.NewForceToken()
		if tok == "" {
			t.Fatal("NewForceToken returned an empty token")
		}
		if _, dup := seen[tok]; dup {
			t.Fatalf("NewForceToken collided: %q", tok)
		}
		seen[tok] = struct{}{}
	}
}

const forceAnnotationKey = "stages.metio.wtf/force"

func uTyped(apiVersion, kind, name, namespace string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion(apiVersion)
	u.SetKind(kind)
	u.SetName(name)
	if namespace != "" {
		u.SetNamespace(namespace)
	}
	return u
}

// A rule whose Target pins APIVersion / Namespace exercises those two matcher
// branches: an object with a different apiVersion or namespace falls through to
// the effective default (Fail) and is left unstamped, while the exact match is
// force-recreated.
func TestResolveConflictHandling_TargetAPIVersionAndNamespaceMatching(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		target      stagesv1.ConflictTarget
		obj         *unstructured.Unstructured
		wantStamped bool
	}{
		{
			name:        "apiVersion mismatch falls through to Fail",
			target:      stagesv1.ConflictTarget{APIVersion: "apps/v1", Kind: "Deployment"},
			obj:         uTyped("v1", "Deployment", "d", "ns"),
			wantStamped: false,
		},
		{
			name:        "apiVersion match recreates",
			target:      stagesv1.ConflictTarget{APIVersion: "apps/v1", Kind: "Deployment"},
			obj:         uTyped("apps/v1", "Deployment", "d", "ns"),
			wantStamped: true,
		},
		{
			name:        "namespace mismatch falls through to Fail",
			target:      stagesv1.ConflictTarget{Namespace: "prod"},
			obj:         uTyped("v1", "ConfigMap", "cm", "staging"),
			wantStamped: false,
		},
		{
			name:        "namespace match recreates",
			target:      stagesv1.ConflictTarget{Namespace: "prod"},
			obj:         uTyped("v1", "ConfigMap", "cm", "prod"),
			wantStamped: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stage := &stagesv1.Stage{ConflictPolicy: &stagesv1.ConflictPolicy{
				Rules: []stagesv1.ConflictRule{{Target: tc.target, Action: "Recreate"}},
			}}
			ch, err := apply.ResolveConflictHandling([]*unstructured.Unstructured{tc.obj}, stage, "tok-test")
			if err != nil {
				t.Fatalf("ResolveConflictHandling: %v", err)
			}
			stamped := tc.obj.GetAnnotations()[forceAnnotationKey] != ""
			if stamped != tc.wantStamped {
				t.Fatalf("stamped = %v, want %v (selector=%+v)", stamped, tc.wantStamped, ch.ForceSelector)
			}
		})
	}
}

// isStatefulData (reached via the rule-Recreate data-loss guard) only fires for
// core/v1 PVC and PV. A non-v1 apiVersion and a v1 non-stateful kind both fall
// out of the guard, so a rule-driven Recreate on them succeeds without
// allowDataLoss.
func TestResolveConflictHandling_StatefulDataGuardScope(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		obj       *unstructured.Unstructured
		wantError bool
	}{
		{
			name:      "core/v1 PVC without allowDataLoss is guarded",
			obj:       uTyped("v1", "PersistentVolumeClaim", "data", "ns"),
			wantError: true,
		},
		{
			name:      "core/v1 PV without allowDataLoss is guarded",
			obj:       uTyped("v1", "PersistentVolume", "vol", ""),
			wantError: true,
		},
		{
			name:      "v1 non-stateful kind is not guarded",
			obj:       uTyped("v1", "ConfigMap", "cm", "ns"),
			wantError: false,
		},
		{
			name:      "non-v1 apiVersion with a PVC-like kind is not guarded",
			obj:       uTyped("storage.example/v1", "PersistentVolumeClaim", "data", "ns"),
			wantError: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stage := &stagesv1.Stage{ConflictPolicy: &stagesv1.ConflictPolicy{
				Rules: []stagesv1.ConflictRule{{
					Target: stagesv1.ConflictTarget{Kind: tc.obj.GetKind()},
					Action: "Recreate",
				}},
			}}
			_, err := apply.ResolveConflictHandling([]*unstructured.Unstructured{tc.obj}, stage, "tok-test")
			if tc.wantError && err == nil {
				t.Fatal("expected the data-loss guard to refuse the recreate")
			}
			if !tc.wantError && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// Wait blocks until the applied object's set is reported ready by kstatus. A
// ConfigMap is immediately current, so Wait returns nil; this drives the Wait
// path against a real apiserver.
func TestWait_ReadyConfigMapReturnsNil(t *testing.T) {
	if testCfg == nil {
		t.Skip("envtest assets unavailable")
	}
	a, _ := applierFor(t)
	ctx := context.Background()

	cm := configMap("waitcfg", map[string]any{"k": "v"})
	cs, err := a.Apply(ctx, "ss", "default", []*unstructured.Unstructured{cm}, apply.ConflictHandling{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	set := make(object.ObjMetadataSet, 0, len(cs.Entries))
	for _, e := range cs.Entries {
		set = append(set, e.ObjMetadata)
	}

	if err := a.Wait(ctx, set, 30*time.Second); err != nil {
		t.Fatalf("Wait on a ready ConfigMap should return nil, got %v", err)
	}
}

// An already-cancelled context makes Wait return promptly with an error rather
// than blocking for the full timeout.
func TestWait_CancelledContextReturnsError(t *testing.T) {
	if testCfg == nil {
		t.Skip("envtest assets unavailable")
	}
	a, _ := applierFor(t)

	cm := configMap("waitcancel", map[string]any{"k": "v"})
	if _, err := a.Apply(context.Background(), "ss", "default", []*unstructured.Unstructured{cm}, apply.ConflictHandling{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	set := object.ObjMetadataSet{{
		Namespace: "default",
		Name:      "waitcancel",
		GroupKind: cm.GroupVersionKind().GroupKind(),
	}}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.Wait(ctx, set, time.Minute); err == nil {
		t.Fatal("Wait with a cancelled context should return an error")
	}
}
