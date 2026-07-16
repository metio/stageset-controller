// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"testing"
	"time"

	"github.com/fluxcd/pkg/apis/meta"
	appsv1 "k8s.io/api/apps/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

func readyChecksStage(rc *stagesv1.ReadyChecks) *stagesv1.Stage {
	return &stagesv1.Stage{Name: "s", SourceRef: stagesv1.SourceReference{Name: "x"}, ReadyChecks: rc}
}

// scopedMapper stands in for the target cluster's RESTMapper with just the
// kinds these tests reference, half namespaced and half cluster-scoped.
func scopedMapper() apimeta.RESTMapper {
	m := apimeta.NewDefaultRESTMapper(nil)
	m.Add(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}, apimeta.RESTScopeNamespace)
	m.Add(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Service"}, apimeta.RESTScopeNamespace)
	m.Add(schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"}, apimeta.RESTScopeRoot)
	m.Add(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Namespace"}, apimeta.RESTScopeRoot)
	m.Add(schema.GroupVersionKind{Group: "storage.k8s.io", Version: "v1", Kind: "StorageClass"}, apimeta.RESTScopeRoot)
	return m
}

func TestReadyCheckObjects_ConvertsAndDefaultsNamespace(t *testing.T) {
	ss := &stagesv1.StageSet{ObjectMeta: metav1.ObjectMeta{Namespace: "ssns"}}
	stage := readyChecksStage(&stagesv1.ReadyChecks{Checks: []meta.NamespacedObjectKindReference{
		{APIVersion: "apps/v1", Kind: "Deployment", Name: "web"},                // empty ns → ss namespace
		{APIVersion: "v1", Kind: "Service", Name: "svc", Namespace: "explicit"}, // explicit ns kept
	}})
	set := readyCheckObjects(scopedMapper(), ss, stage)
	if len(set) != 2 {
		t.Fatalf("got %d objects, want 2", len(set))
	}
	if set[0].Namespace != "ssns" || set[0].GroupKind.Group != "apps" || set[0].GroupKind.Kind != "Deployment" {
		t.Errorf("first object = %+v, want apps/Deployment in ssns", set[0])
	}
	if set[1].Namespace != "explicit" || set[1].GroupKind.Group != "" {
		t.Errorf("second object = %+v, want core/Service in explicit", set[1])
	}
}

// A cluster-scoped check must produce a namespace-less key. kstatus matches on
// the whole ObjMetadata, and a cluster-scoped object carries no namespace — so a
// namespaced key matches nothing and the stage waits out its verify timeout on
// an object that has been Established all along. This is the "operator installs
// the CRDs, a later stage applies the CRs" ordering, which is most of the reason
// to reach for readyChecks at all.
func TestReadyCheckObjects_ClusterScopedKindsCarryNoNamespace(t *testing.T) {
	ss := &stagesv1.StageSet{ObjectMeta: metav1.ObjectMeta{Namespace: "ssns"}}
	stage := readyChecksStage(&stagesv1.ReadyChecks{Checks: []meta.NamespacedObjectKindReference{
		{APIVersion: "apiextensions.k8s.io/v1", Kind: "CustomResourceDefinition", Name: "clusters.postgresql.cnpg.io"},
		{APIVersion: "v1", Kind: "Namespace", Name: "team-a"},
		{APIVersion: "storage.k8s.io/v1", Kind: "StorageClass", Name: "fast"},
	}})

	set := readyCheckObjects(scopedMapper(), ss, stage)
	if len(set) != 3 {
		t.Fatalf("got %d objects, want 3", len(set))
	}
	for _, o := range set {
		if o.Namespace != "" {
			t.Errorf("%s/%s got namespace %q, want empty (cluster-scoped)", o.GroupKind.Kind, o.Name, o.Namespace)
		}
	}
}

// A namespace on a cluster-scoped kind is meaningless rather than merely
// redundant: honoring it would build a key nothing can occupy. Admission does
// not reject it (scope belongs to the target cluster), so the resolve drops it.
func TestReadyCheckObjects_ClusterScopedIgnoresAnExplicitNamespace(t *testing.T) {
	ss := &stagesv1.StageSet{ObjectMeta: metav1.ObjectMeta{Namespace: "ssns"}}
	stage := readyChecksStage(&stagesv1.ReadyChecks{Checks: []meta.NamespacedObjectKindReference{
		{APIVersion: "apiextensions.k8s.io/v1", Kind: "CustomResourceDefinition", Name: "widgets.example.com", Namespace: "wrong"},
	}})

	set := readyCheckObjects(scopedMapper(), ss, stage)
	if set[0].Namespace != "" {
		t.Errorf("namespace = %q, want it dropped for a cluster-scoped kind", set[0].Namespace)
	}
}

// A kind the target cluster does not serve cannot be scoped, so the namespaced
// default stands. The check then reports Unknown — the honest answer — rather
// than gating on a key that could never be occupied either way.
func TestReadyCheckObjects_UnresolvableKindFallsBackToNamespaced(t *testing.T) {
	ss := &stagesv1.StageSet{ObjectMeta: metav1.ObjectMeta{Namespace: "ssns"}}
	stage := readyChecksStage(&stagesv1.ReadyChecks{Checks: []meta.NamespacedObjectKindReference{
		{APIVersion: "nosuch.example.com/v1", Kind: "Mystery", Name: "m"},
	}})

	for name, mapper := range map[string]apimeta.RESTMapper{"unknown kind": scopedMapper(), "nil mapper": nil} {
		t.Run(name, func(t *testing.T) {
			set := readyCheckObjects(mapper, ss, stage)
			if set[0].Namespace != "ssns" {
				t.Errorf("namespace = %q, want the StageSet's as the fallback", set[0].Namespace)
			}
		})
	}
}

func TestCompileReadyExprs_RejectsMalformedCEL(t *testing.T) {
	stage := readyChecksStage(&stagesv1.ReadyChecks{Exprs: []stagesv1.CustomHealthCheck{
		{APIVersion: "apps/v1", Kind: "Deployment", Current: "this is not ( valid CEL"},
	}})
	if _, err := compileReadyExprs(stage); err == nil {
		t.Fatal("compileReadyExprs accepted malformed CEL, want error")
	}
}

func TestValidateReadyChecks(t *testing.T) {
	tests := []struct {
		name    string
		rc      *stagesv1.ReadyChecks
		wantErr bool
	}{
		{name: "nil ok", rc: nil},
		{name: "valid current expr", rc: &stagesv1.ReadyChecks{Exprs: []stagesv1.CustomHealthCheck{{APIVersion: "apps/v1", Kind: "Deployment", Current: "status.readyReplicas == spec.replicas"}}}},
		{name: "malformed expr rejected", rc: &stagesv1.ReadyChecks{Exprs: []stagesv1.CustomHealthCheck{{APIVersion: "apps/v1", Kind: "Deployment", Current: "@@"}}}, wantErr: true},
		{name: "check missing kind rejected", rc: &stagesv1.ReadyChecks{Checks: []meta.NamespacedObjectKindReference{{Name: "x"}}}, wantErr: true},
		{name: "check missing name rejected", rc: &stagesv1.ReadyChecks{Checks: []meta.NamespacedObjectKindReference{{Kind: "Deployment"}}}, wantErr: true},
		{name: "valid check ok", rc: &stagesv1.ReadyChecks{Checks: []meta.NamespacedObjectKindReference{{Kind: "Deployment", Name: "x"}}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ss := &stagesv1.StageSet{Spec: stagesv1.StageSetSpec{Stages: []stagesv1.Stage{*readyChecksStage(tc.rc)}}}
			if err := validateReadyChecks(ss); (err != nil) != tc.wantErr {
				t.Errorf("validateReadyChecks err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

func deploymentWith(name string, replicas, ready, unavailable int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       appsv1.DeploymentSpec{Replicas: new(replicas)},
		Status:     appsv1.DeploymentStatus{ReadyReplicas: ready, UnavailableReplicas: unavailable},
	}
}

func appliedDeploymentRef(name string) *unstructured.Unstructured {
	o := &unstructured.Unstructured{}
	o.SetGroupVersionKind(appsv1.SchemeGroupVersion.WithKind("Deployment"))
	o.SetNamespace("default")
	o.SetName(name)
	return o
}

func TestEvalReadyExprs_CurrentSatisfied(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(builderScheme(t)).WithObjects(deploymentWith("web", 3, 3, 0)).Build()
	stage := readyChecksStage(&stagesv1.ReadyChecks{Exprs: []stagesv1.CustomHealthCheck{
		{APIVersion: "apps/v1", Kind: "Deployment", Current: "status.readyReplicas == spec.replicas"},
	}})
	ss := &stagesv1.StageSet{ObjectMeta: metav1.ObjectMeta{Namespace: "default"}}
	if err := evalReadyExprs(context.Background(), c, ss, stage, []*unstructured.Unstructured{appliedDeploymentRef("web")}, time.Second); err != nil {
		t.Fatalf("Current-satisfied check should pass, got %v", err)
	}
}

func TestEvalReadyExprs_CurrentFalseTimesOut(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(builderScheme(t)).WithObjects(deploymentWith("web", 3, 1, 0)).Build()
	stage := readyChecksStage(&stagesv1.ReadyChecks{Exprs: []stagesv1.CustomHealthCheck{
		{APIVersion: "apps/v1", Kind: "Deployment", Current: "status.readyReplicas == spec.replicas"},
	}})
	ss := &stagesv1.StageSet{ObjectMeta: metav1.ObjectMeta{Namespace: "default"}}
	if err := evalReadyExprs(context.Background(), c, ss, stage, []*unstructured.Unstructured{appliedDeploymentRef("web")}, 200*time.Millisecond); err == nil {
		t.Fatal("Current-false check should not become ready, want timeout error")
	}
}

func TestEvalReadyExprs_FailedTrips(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(builderScheme(t)).WithObjects(deploymentWith("web", 3, 0, 2)).Build()
	stage := readyChecksStage(&stagesv1.ReadyChecks{Exprs: []stagesv1.CustomHealthCheck{
		{APIVersion: "apps/v1", Kind: "Deployment", Current: "status.readyReplicas == spec.replicas", Failed: "status.unavailableReplicas > 0"},
	}})
	ss := &stagesv1.StageSet{ObjectMeta: metav1.ObjectMeta{Namespace: "default"}}
	if err := evalReadyExprs(context.Background(), c, ss, stage, []*unstructured.Unstructured{appliedDeploymentRef("web")}, time.Second); err == nil {
		t.Fatal("a tripped Failed expression should fail the check, got nil")
	}
}
