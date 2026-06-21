// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package apply

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

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
		ch, err := ResolveConflictHandling([]*unstructured.Unstructured{obj}, &stagesv1.Stage{}, "tok-test")
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

	t.Run("stage.force: Recreate fallback stamps force with the selector token", func(t *testing.T) {
		t.Parallel()
		obj := uobj("v1", "ConfigMap", "cm")
		ch, err := ResolveConflictHandling([]*unstructured.Unstructured{obj}, &stagesv1.Stage{Force: true}, "tok-test")
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		token := ch.ForceSelector[forceAnnotation]
		if token == "" {
			t.Fatalf("stage.force should set ForceSelector, got %+v", ch)
		}
		// The stamp on the object and the selector value must be the same token,
		// so the desired object matches the selector this pass.
		if obj.GetAnnotations()[forceAnnotation] != token {
			t.Fatalf("force stamp %q must equal selector token %q",
				obj.GetAnnotations()[forceAnnotation], token)
		}
	})

	t.Run("rule Recreate on one kind leaves others on Fail", func(t *testing.T) {
		t.Parallel()
		secret := uobj("v1", "Secret", "s")
		cm := uobj("v1", "ConfigMap", "cm")
		stage := &stagesv1.Stage{ConflictPolicy: &stagesv1.ConflictPolicy{
			Rules: []stagesv1.ConflictRule{recreateRule("Secret", "", false)},
		}}
		ch, err := ResolveConflictHandling([]*unstructured.Unstructured{secret, cm}, stage, "tok-test")
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if ch.ForceSelector == nil {
			t.Fatal("Secret rule should set ForceSelector")
		}
		if secret.GetAnnotations()[forceAnnotation] != ch.ForceSelector[forceAnnotation] {
			t.Fatal("Secret should be force-stamped with the selector token")
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
		ch, err := ResolveConflictHandling([]*unstructured.Unstructured{cm}, stage, "tok-test")
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
		if _, err := ResolveConflictHandling([]*unstructured.Unstructured{pvc}, stage, "tok-test"); err == nil {
			t.Fatal("recreating a PVC without allowDataLoss must be refused")
		}
	})

	t.Run("rule Recreate on PVC with allowDataLoss is permitted", func(t *testing.T) {
		t.Parallel()
		pvc := uobj("v1", "PersistentVolumeClaim", "data")
		stage := &stagesv1.Stage{ConflictPolicy: &stagesv1.ConflictPolicy{
			Rules: []stagesv1.ConflictRule{recreateRule("PersistentVolumeClaim", "", true)},
		}}
		ch, err := ResolveConflictHandling([]*unstructured.Unstructured{pvc}, stage, "tok-test")
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
		if _, err := ResolveConflictHandling([]*unstructured.Unstructured{pvc}, &stagesv1.Stage{Force: true}, "tok-test"); err != nil {
			t.Fatalf("blunt force is an explicit opt-in and must not be gated, err = %v", err)
		}
	})

	t.Run("per-object force annotation wins over policy default Fail", func(t *testing.T) {
		t.Parallel()
		obj := uobj("v1", "ConfigMap", "cm")
		obj.SetAnnotations(map[string]string{forceAnnotation: "operator-set"})
		stage := &stagesv1.Stage{ConflictPolicy: &stagesv1.ConflictPolicy{Default: conflictFail}}
		ch, err := ResolveConflictHandling([]*unstructured.Unstructured{obj}, stage, "tok-test")
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if ch.ForceSelector == nil {
			t.Fatal("the force annotation must win over default Fail")
		}
	})
}
