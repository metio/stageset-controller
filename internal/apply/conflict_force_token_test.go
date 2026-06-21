// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package apply

import (
	"testing"

	"github.com/fluxcd/pkg/ssa/utils"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

// The default crypto/rand seam produces a fresh, non-empty token on each call,
// so two consecutive applies never share a force selector value.
func TestForceToken_FreshPerCall(t *testing.T) {
	t.Parallel()
	seen := map[string]struct{}{}
	for range 100 {
		tok := newForceToken()
		if tok == "" {
			t.Fatal("token must be non-empty")
		}
		if _, dup := seen[tok]; dup {
			t.Fatalf("token %q collided across calls", tok)
		}
		seen[tok] = struct{}{}
	}
}

// All Recreate objects in one apply share the supplied token, and both the stamp
// and the ForceSelector carry it — so a different token on a later apply yields a
// distinct selector.
func TestResolveConflictHandling_StampsSuppliedToken(t *testing.T) {
	t.Parallel()

	stage := &stagesv1.Stage{Force: true}

	a := uobj("v1", "ConfigMap", "a")
	b := uobj("v1", "ConfigMap", "b")
	ch1, err := ResolveConflictHandling([]*unstructured.Unstructured{a, b}, stage, "tok-1")
	if err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	if ch1.ForceSelector[forceAnnotation] != "tok-1" {
		t.Fatalf("pass 1 selector = %q, want tok-1", ch1.ForceSelector[forceAnnotation])
	}
	if a.GetAnnotations()[forceAnnotation] != "tok-1" || b.GetAnnotations()[forceAnnotation] != "tok-1" {
		t.Fatal("every Recreate object in one apply must carry the apply's token")
	}

	c := uobj("v1", "ConfigMap", "a")
	ch2, err := ResolveConflictHandling([]*unstructured.Unstructured{c}, stage, "tok-2")
	if err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	if ch2.ForceSelector[forceAnnotation] != "tok-2" {
		t.Fatalf("pass 2 selector = %q, want tok-2", ch2.ForceSelector[forceAnnotation])
	}
}

// Fail and KeepExisting objects are never force-stamped.
func TestResolveConflictHandling_NoForceStampOnFailOrKeep(t *testing.T) {
	t.Parallel()

	failObj := uobj("v1", "ConfigMap", "fail") // default policy is Fail
	keepObj := uobj("v1", "ConfigMap", "keep")
	stage := &stagesv1.Stage{ConflictPolicy: &stagesv1.ConflictPolicy{
		Rules: []stagesv1.ConflictRule{
			{Target: stagesv1.ConflictTarget{Name: "keep"}, Action: conflictKeepExisting},
		},
	}}

	if _, err := ResolveConflictHandling([]*unstructured.Unstructured{failObj, keepObj}, stage, "tok-test"); err != nil {
		t.Fatalf("err = %v", err)
	}
	if _, ok := failObj.GetAnnotations()[forceAnnotation]; ok {
		t.Fatal("a Fail object must not carry a force annotation")
	}
	if _, ok := keepObj.GetAnnotations()[forceAnnotation]; ok {
		t.Fatal("a KeepExisting object must not carry a force annotation")
	}
	if keepObj.GetAnnotations()[keepExistingAnnotation] != keepExistingValue {
		t.Fatal("a KeepExisting object must carry the keep-existing annotation")
	}
}

// The ssa-matching invariant, asserted against the real matcher ssa's
// shouldForceApply uses (utils.AnyInMetadata): a live object bearing a STALE
// force token does NOT match a ForceSelector built with a fresh token, while a
// this-pass-stamped object DOES.
func TestForceSelector_StaleTokenDoesNotMatch(t *testing.T) {
	t.Parallel()

	// Pass 1 stamps a stale token onto what becomes the live object.
	live := uobj("v1", "PersistentVolumeClaim", "data")
	if _, err := ResolveConflictHandling([]*unstructured.Unstructured{live}, &stagesv1.Stage{Force: true}, "stale-0000"); err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	if live.GetAnnotations()[forceAnnotation] != "stale-0000" {
		t.Fatalf("pass 1 stamp = %q", live.GetAnnotations()[forceAnnotation])
	}

	// Pass 2 uses a fresh token; its selector is what ssa would match against the
	// live object's stale annotation.
	desired := uobj("v1", "PersistentVolumeClaim", "data")
	ch2, err := ResolveConflictHandling([]*unstructured.Unstructured{desired}, &stagesv1.Stage{Force: true}, "fresh-1111")
	if err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	freshSelector := ch2.ForceSelector

	// The live object still carries the stale token: ssa's matcher must NOT match
	// it against pass 2's fresh selector — no spurious force/recreate.
	if utils.AnyInMetadata(live, freshSelector) {
		t.Fatal("stale-token live object must not match a fresh-token ForceSelector")
	}

	// The this-pass desired object carries the fresh token: it MUST match, so a
	// current-pass Recreate still recreates.
	if !utils.AnyInMetadata(desired, freshSelector) {
		t.Fatal("this-pass desired object must match its own fresh ForceSelector")
	}
}
