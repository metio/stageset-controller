// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

// recreateRule builds a Recreate conflict rule for the given target. "Recreate"
// is the CRD enum value (apply.ResolveConflictHandling matches it); spelled as a
// literal here because the apply package's action consts are unexported.
func recreateRule(kind, name string, allowDataLoss bool) stagesv1.ConflictRule {
	return stagesv1.ConflictRule{
		Target:        stagesv1.ConflictTarget{Kind: kind, Name: name},
		Action:        "Recreate",
		AllowDataLoss: allowDataLoss,
	}
}

func immutableConfigMapManifest(ns, name, val string) string {
	return "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: " + name + "\n  namespace: " + ns +
		"\nimmutable: true\ndata:\n  key: " + val + "\n"
}

func cmDataKey(t *testing.T, c client.Client, ns, name string) string {
	t.Helper()
	var cm corev1.ConfigMap
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &cm); err != nil {
		t.Fatalf("get ConfigMap %q: %v", name, err)
	}
	return cm.Data["key"]
}

func conflictStageSet(t *testing.T, c client.Client, ns, name, eaName string, policy *stagesv1.ConflictPolicy) *stagesv1.StageSet {
	t.Helper()
	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: stagesv1.StageSetSpec{
			Interval: metav1.Duration{Duration: 5 * time.Minute},
			Stages:   []stagesv1.Stage{{Name: "stage-a", SourceRef: stagesv1.SourceReference{Name: eaName}, ConflictPolicy: policy}},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	return ss
}

// Without a conflictPolicy, an immutable-field change is rejected by the
// apiserver and the stage fails — the object keeps its old value.
func TestReconcile_Conflict_ImmutableFailsWithoutPolicy(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	servedArtifact(t, c, ns, "cm-art", "", map[string]string{"cm.yaml": immutableConfigMapManifest(ns, "cfg", "v1")})

	ss := conflictStageSet(t, c, ns, "nopolicy", "cm-art", nil)
	reconcileOnce(t, c, ss)
	if cmDataKey(t, c, ns, "cfg") != "v1" {
		t.Fatal("first apply should create the immutable ConfigMap at v1")
	}

	repointArtifact(t, c, ns, "cm-art", map[string]string{"cm.yaml": immutableConfigMapManifest(ns, "cfg", "v2")})
	_ = reconcileWith(t, c, ss, nil)

	if r := readyReason(getStageSet(t, c, ns, "nopolicy")); r != ReasonStageFailed {
		t.Fatalf("Ready reason = %q, want %q", r, ReasonStageFailed)
	}
	if got := cmDataKey(t, c, ns, "cfg"); got != "v1" {
		t.Fatalf("immutable ConfigMap should be unchanged, got key=%q", got)
	}
}

// A conflictPolicy Recreate rule deletes and re-applies the object on an
// immutable-field conflict, so the new value lands.
func TestReconcile_Conflict_RecreatePolicyAppliesNewValue(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	servedArtifact(t, c, ns, "cm-art", "", map[string]string{"cm.yaml": immutableConfigMapManifest(ns, "cfg", "v1")})

	policy := &stagesv1.ConflictPolicy{Rules: []stagesv1.ConflictRule{recreateRule("ConfigMap", "", false)}}
	ss := conflictStageSet(t, c, ns, "recreate", "cm-art", policy)
	reconcileOnce(t, c, ss)
	if cmDataKey(t, c, ns, "cfg") != "v1" {
		t.Fatal("first apply should create the immutable ConfigMap at v1")
	}

	repointArtifact(t, c, ns, "cm-art", map[string]string{"cm.yaml": immutableConfigMapManifest(ns, "cfg", "v2")})
	_ = reconcileWith(t, c, ss, nil)

	if r := readyReason(getStageSet(t, c, ns, "recreate")); r != ReasonReady {
		t.Fatalf("Ready reason = %q, want %q", r, ReasonReady)
	}
	if got := cmDataKey(t, c, ns, "cfg"); got != "v2" {
		t.Fatalf("Recreate policy should have re-applied the new value, got key=%q", got)
	}
}
