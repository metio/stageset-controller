// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

// Pruning re-checks the live object's owner labels: an object adopted or
// relabeled by another manager between reconciles is left alone, while a
// still-owned object that fell out of the render is deleted as usual.
func TestReconcile_PruneSkipsObjectsNoLongerOwned(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)

	servedArtifact(t, c, ns, "bundle", "", map[string]string{
		"a.yaml":    configMapManifest(ns, "obj-a"),
		"b.yaml":    configMapManifest(ns, "obj-b"),
		"keep.yaml": configMapManifest(ns, "obj-keep"),
	})
	ss := newStageSet(t, c, ns, "owner-prune", stagesv1.SourceReference{Name: "bundle"})
	reconcileOnce(t, c, ss)
	if !cmExists(t, c, ns, "obj-a") || !cmExists(t, c, ns, "obj-b") {
		t.Fatal("first run should apply both ConfigMaps")
	}

	// Another manager adopts obj-b: strip the StageSet owner labels from its
	// LIVE object.
	var liveB corev1.ConfigMap
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "obj-b"}, &liveB); err != nil {
		t.Fatalf("get obj-b: %v", err)
	}
	labels := liveB.GetLabels()
	delete(labels, "stages.metio.wtf/name")
	delete(labels, "stages.metio.wtf/namespace")
	liveB.SetLabels(labels)
	if err := c.Update(context.Background(), &liveB); err != nil {
		t.Fatalf("strip owner labels from obj-b: %v", err)
	}

	// Run 2: the render drops obj-a and obj-b, so both are pruned by inventory.
	// A third object keeps the artifact non-empty — an artifact with no manifest
	// files at all is refused (build.ErrNoManifests) rather than read as "prune
	// everything", since the usual cause is a source that shipped nothing.
	repointArtifact(t, c, ns, "bundle", map[string]string{
		"keep.yaml": configMapManifest(ns, "obj-keep"),
	})
	reconcileOnce(t, c, ss)

	// obj-a was still owned and is pruned; obj-b was relabeled and survives.
	if cmExists(t, c, ns, "obj-a") {
		t.Fatal("obj-a (still owned) should have been pruned")
	}
	if !cmExists(t, c, ns, "obj-b") {
		t.Fatal("obj-b (ownership transferred) must survive the prune")
	}
}
