// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

// twoCMStageSet builds a single-stage StageSet whose stage applies two immutable
// ConfigMaps and carries the given conflict policy.
func twoCMStageSet(t *testing.T, c client.Client, ns, name, eaName string, policy *stagesv1.ConflictPolicy) *stagesv1.StageSet {
	t.Helper()
	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: stagesv1.StageSetSpec{
			Interval: metav1.Duration{Duration: 5 * time.Minute},
			Stages: []stagesv1.Stage{{
				Name:           "stage-a",
				SourceRef:      stagesv1.SourceReference{Name: eaName},
				ConflictPolicy: policy,
			}},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	return ss
}

// A stale `stages.metio.wtf/force` annotation left on a live object from an
// earlier Recreate pass must NOT cause a force-recreate when the object's
// current policy is Fail, even if a sibling in the same stage is Recreate this
// pass. The per-apply force token rotates, so the live object's stale token can
// never match the current ForceSelector — the Fail object's immutable conflict
// fails the stage and the object keeps its data.
func TestReconcile_Conflict_StaleForceTokenDoesNotRecreateFailObject(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)

	const failName, forceName = "fail-cm", "force-cm"
	v1files := map[string]string{
		"fail.yaml":  immutableConfigMapManifest(ns, failName, "v1"),
		"force.yaml": immutableConfigMapManifest(ns, forceName, "v1"),
	}
	servedArtifact(t, c, ns, "cm-art", "", v1files)

	// Pass 1: both ConfigMaps recreate-eligible, so both live objects get
	// stamped with this apply's force token.
	recreateAll := &stagesv1.ConflictPolicy{
		Rules: []stagesv1.ConflictRule{recreateRule("ConfigMap", "", false)},
	}
	ss := twoCMStageSet(t, c, ns, "stale-force", "cm-art", recreateAll)
	reconcileOnce(t, c, ss)
	if cmDataKey(t, c, ns, failName) != "v1" || cmDataKey(t, c, ns, forceName) != "v1" {
		t.Fatal("first apply should create both ConfigMaps at v1")
	}

	// Switch policy: fail-cm is now Fail, force-cm stays Recreate. The live
	// fail-cm still carries pass 1's (now stale) force annotation.
	mixed := &stagesv1.ConflictPolicy{
		Default: "Fail",
		Rules: []stagesv1.ConflictRule{
			{Target: stagesv1.ConflictTarget{Name: forceName}, Action: "Recreate"},
		},
	}
	fresh := getStageSet(t, c, ns, "stale-force")
	fresh.Spec.Stages[0].ConflictPolicy = mixed
	if err := c.Update(context.Background(), fresh); err != nil {
		t.Fatalf("update StageSet policy: %v", err)
	}

	// Bump fail-cm to v2 — an immutable-field change the apiserver rejects.
	// force-cm stays at v1 so the only conflict this pass is fail-cm's; with a
	// constant force selector, fail-cm's stale annotation (plus a sibling on
	// Recreate) would have matched and force-deleted it.
	repointArtifact(t, c, ns, "cm-art", map[string]string{
		"fail.yaml":  immutableConfigMapManifest(ns, failName, "v2"),
		"force.yaml": immutableConfigMapManifest(ns, forceName, "v1"),
	})
	_ = reconcileWith(t, c, fresh, nil)

	// The stage must fail on fail-cm's immutable conflict — the stale force
	// annotation must NOT engage a recreate.
	if r := readyReason(getStageSet(t, c, ns, "stale-force")); r != ReasonStageFailed {
		t.Fatalf("Ready reason = %q, want %q", r, ReasonStageFailed)
	}
	// fail-cm is Fail this pass: the stale force annotation on the live object
	// must NOT match the fresh selector, so it is not force-recreated and keeps
	// its v1 data.
	if got := cmDataKey(t, c, ns, failName); got != "v1" {
		t.Fatalf("fail-cm must NOT be recreated (stale token must not match); got key=%q", got)
	}
}
