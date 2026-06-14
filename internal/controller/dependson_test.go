// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"testing"
	"time"

	"github.com/fluxcd/pkg/apis/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

func stageSetDependsOn(t *testing.T, c client.Client, ns, name, eaName string, deps ...string) *stagesv1.StageSet {
	t.Helper()
	depRefs := make([]meta.NamespacedObjectReference, 0, len(deps))
	for _, d := range deps {
		depRefs = append(depRefs, meta.NamespacedObjectReference{Name: d})
	}
	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: stagesv1.StageSetSpec{
			Interval:  metav1.Duration{Duration: time.Minute},
			DependsOn: depRefs,
			Stages:    []stagesv1.Stage{{Name: "stage-a", SourceRef: stagesv1.SourceReference{Name: eaName}}},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	return ss
}

func TestReconcile_DependencyNotReady(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	servedArtifact(t, c, ns, "ea", "", map[string]string{"cm.yaml": configMapManifest(ns, "gated")})

	ss := stageSetDependsOn(t, c, ns, "dependent", "ea", "missing-dep")
	reconcileOnce(t, c, ss)

	if r := readyReason(getStageSet(t, c, ns, "dependent")); r != ReasonDependencyNotReady {
		t.Fatalf("Ready reason = %q, want %q", r, ReasonDependencyNotReady)
	}
	if cmExists(t, c, ns, "gated") {
		t.Fatal("a gated StageSet must not apply its stages")
	}
}

func TestReconcile_DependencyReady(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)

	servedArtifact(t, c, ns, "ea-a", "", map[string]string{"a.yaml": configMapManifest(ns, "dep-obj")})
	a := newStageSet(t, c, ns, "dep-a", stagesv1.SourceReference{Name: "ea-a"})
	reconcileOnce(t, c, a)
	if readyReason(getStageSet(t, c, ns, "dep-a")) != ReasonReady {
		t.Fatal("the dependency should be Ready after a successful run")
	}

	servedArtifact(t, c, ns, "ea-b", "", map[string]string{"b.yaml": configMapManifest(ns, "dependent-obj")})
	b := stageSetDependsOn(t, c, ns, "dep-b", "ea-b", "dep-a")
	reconcileOnce(t, c, b)

	if r := readyReason(getStageSet(t, c, ns, "dep-b")); r != ReasonReady {
		t.Fatalf("dependent should proceed once its dependency is Ready, reason = %q", r)
	}
	if !cmExists(t, c, ns, "dependent-obj") {
		t.Fatal("dependent should apply once the gate clears")
	}
}

func TestReconcile_DependencyCycle(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	servedArtifact(t, c, ns, "ea-x", "", map[string]string{"x.yaml": configMapManifest(ns, "cyc-x")})
	servedArtifact(t, c, ns, "ea-y", "", map[string]string{"y.yaml": configMapManifest(ns, "cyc-y")})

	stageSetDependsOn(t, c, ns, "cyc-x", "ea-x", "cyc-y")
	y := stageSetDependsOn(t, c, ns, "cyc-y", "ea-y", "cyc-x")
	reconcileOnce(t, c, y)

	if r := readyReason(getStageSet(t, c, ns, "cyc-y")); r != ReasonStalled {
		t.Fatalf("a dependsOn cycle should Stall, reason = %q", r)
	}
}
