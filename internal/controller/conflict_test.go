// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

func uobj(apiVersion, kind, name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion(apiVersion)
	u.SetKind(kind)
	u.SetName(name)
	return u
}

func recreateRule(kind, name string, allowDataLoss bool) stagesv1.ConflictRule {
	return stagesv1.ConflictRule{
		Target:        stagesv1.ConflictTarget{Kind: kind, Name: name},
		Action:        conflictRecreate,
		AllowDataLoss: allowDataLoss,
	}
}

func TestResolveConflictHandling(t *testing.T) {
	t.Parallel()

	t.Run("default Fail: no selectors, no annotations", func(t *testing.T) {
		t.Parallel()
		obj := uobj("v1", "ConfigMap", "cm")
		ch, err := resolveConflictHandling([]*unstructured.Unstructured{obj}, &stagesv1.Stage{})
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if ch.ForceSelector != nil || ch.IfNotPresentSelector != nil {
			t.Fatalf("Fail should set no selectors, got %+v", ch)
		}
		if obj.GetAnnotations()[forceAnnotation] != "" {
			t.Fatal("Fail should stamp no force annotation")
		}
	})

	t.Run("stage.force: Recreate fallback stamps force", func(t *testing.T) {
		t.Parallel()
		obj := uobj("v1", "ConfigMap", "cm")
		ch, err := resolveConflictHandling([]*unstructured.Unstructured{obj}, &stagesv1.Stage{Force: true})
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if ch.ForceSelector[forceAnnotation] != forceEnabledValue {
			t.Fatalf("stage.force should set ForceSelector, got %+v", ch)
		}
		if obj.GetAnnotations()[forceAnnotation] != forceEnabledValue {
			t.Fatal("stage.force should stamp the force annotation")
		}
	})

	t.Run("rule Recreate on one kind leaves others on Fail", func(t *testing.T) {
		t.Parallel()
		secret := uobj("v1", "Secret", "s")
		cm := uobj("v1", "ConfigMap", "cm")
		stage := &stagesv1.Stage{ConflictPolicy: &stagesv1.ConflictPolicy{
			Rules: []stagesv1.ConflictRule{recreateRule("Secret", "", false)},
		}}
		ch, err := resolveConflictHandling([]*unstructured.Unstructured{secret, cm}, stage)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if ch.ForceSelector == nil {
			t.Fatal("Secret rule should set ForceSelector")
		}
		if secret.GetAnnotations()[forceAnnotation] != forceEnabledValue {
			t.Fatal("Secret should be force-stamped")
		}
		if cm.GetAnnotations()[forceAnnotation] != "" {
			t.Fatal("ConfigMap (default Fail) should not be force-stamped")
		}
	})

	t.Run("rule KeepExisting sets IfNotPresentSelector", func(t *testing.T) {
		t.Parallel()
		cm := uobj("v1", "ConfigMap", "legacy")
		stage := &stagesv1.Stage{ConflictPolicy: &stagesv1.ConflictPolicy{
			Rules: []stagesv1.ConflictRule{{Target: stagesv1.ConflictTarget{Kind: "ConfigMap"}, Action: conflictKeepExisting}},
		}}
		ch, err := resolveConflictHandling([]*unstructured.Unstructured{cm}, stage)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if ch.IfNotPresentSelector[keepExistingAnnotation] != keepExistingValue {
			t.Fatalf("KeepExisting should set IfNotPresentSelector, got %+v", ch)
		}
	})

	t.Run("rule Recreate on PVC without allowDataLoss is refused", func(t *testing.T) {
		t.Parallel()
		pvc := uobj("v1", "PersistentVolumeClaim", "data")
		stage := &stagesv1.Stage{ConflictPolicy: &stagesv1.ConflictPolicy{
			Rules: []stagesv1.ConflictRule{recreateRule("PersistentVolumeClaim", "", false)},
		}}
		if _, err := resolveConflictHandling([]*unstructured.Unstructured{pvc}, stage); err == nil {
			t.Fatal("recreating a PVC without allowDataLoss must be refused")
		}
	})

	t.Run("rule Recreate on PVC with allowDataLoss is permitted", func(t *testing.T) {
		t.Parallel()
		pvc := uobj("v1", "PersistentVolumeClaim", "data")
		stage := &stagesv1.Stage{ConflictPolicy: &stagesv1.ConflictPolicy{
			Rules: []stagesv1.ConflictRule{recreateRule("PersistentVolumeClaim", "", true)},
		}}
		ch, err := resolveConflictHandling([]*unstructured.Unstructured{pvc}, stage)
		if err != nil {
			t.Fatalf("allowDataLoss should permit PVC recreate, err = %v", err)
		}
		if ch.ForceSelector == nil {
			t.Fatal("PVC with allowDataLoss should be force-stamped")
		}
	})

	t.Run("blunt stage.force on a PVC is not gated", func(t *testing.T) {
		t.Parallel()
		pvc := uobj("v1", "PersistentVolumeClaim", "data")
		if _, err := resolveConflictHandling([]*unstructured.Unstructured{pvc}, &stagesv1.Stage{Force: true}); err != nil {
			t.Fatalf("blunt force is an explicit opt-in and must not be gated, err = %v", err)
		}
	})

	t.Run("per-object force annotation wins over policy default Fail", func(t *testing.T) {
		t.Parallel()
		obj := uobj("v1", "ConfigMap", "cm")
		obj.SetAnnotations(map[string]string{forceAnnotation: forceEnabledValue})
		stage := &stagesv1.Stage{ConflictPolicy: &stagesv1.ConflictPolicy{Default: conflictFail}}
		ch, err := resolveConflictHandling([]*unstructured.Unstructured{obj}, stage)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if ch.ForceSelector == nil {
			t.Fatal("the force annotation must win over default Fail")
		}
	})
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
