// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package cli

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

// TestApply_CreatesObjectsWithLabels server-side-applies a stage's rendered
// ConfigMap and asserts it lands in the cluster carrying the same owner and
// per-stage labels a reconcile would stamp.
func TestApply_CreatesObjectsWithLabels(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "applycreate")
	makeStageSet(t, c, ns, "app")
	dir := writeSourceTree(t, map[string]string{
		"cm.yaml": configMapManifest(ns, "settings", map[string]string{"greeting": "hello"}),
	})

	stdout, stderr, code := runCLI(t, cfg, "apply", "app", "-n", ns, "--source-dir", dir)
	if code != exitOK {
		t.Fatalf("apply exit = %d (stderr=%s)\n%s", code, stderr, stdout)
	}
	if !strings.Contains(stdout, "ConfigMap/"+ns+"/settings") {
		t.Errorf("apply output should report the applied ConfigMap:\n%s", stdout)
	}

	var cm corev1.ConfigMap
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "settings"}, &cm); err != nil {
		t.Fatalf("ConfigMap was not applied: %v", err)
	}
	if cm.Data["greeting"] != "hello" {
		t.Errorf("data not applied: %v", cm.Data)
	}
	if cm.Labels["stages.metio.wtf/name"] != "app" {
		t.Errorf("owner label missing: %v", cm.Labels)
	}
	if cm.Labels[stagesv1.StageLabel] != "first" {
		t.Errorf("per-stage label missing: %v", cm.Labels)
	}
}

// TestApply_Idempotent re-applies identical content; the second run reports the
// object unchanged (server-side apply is idempotent).
func TestApply_Idempotent(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "applyidem")
	makeStageSet(t, c, ns, "app")
	dir := writeSourceTree(t, map[string]string{
		"cm.yaml": configMapManifest(ns, "settings", map[string]string{"k": "v"}),
	})

	if _, _, code := runCLI(t, cfg, "apply", "app", "-n", ns, "--source-dir", dir); code != exitOK {
		t.Fatalf("first apply exit = %d", code)
	}
	stdout, _, code := runCLI(t, cfg, "apply", "app", "-n", ns, "--source-dir", dir)
	if code != exitOK {
		t.Fatalf("second apply exit = %d", code)
	}
	if !strings.Contains(stdout, "unchanged ConfigMap/"+ns+"/settings") {
		t.Errorf("re-apply of identical content should report unchanged:\n%s", stdout)
	}
}

// TestApply_StageFilter applies only the named stage of a two-stage StageSet; the
// other stage's object must not be created.
func TestApply_StageFilter(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "applyfilter")
	ss := makeStageSet(t, c, ns, "app")
	ss.Spec.Stages = append(ss.Spec.Stages, stagesv1.Stage{
		Name:      "second",
		SourceRef: stagesv1.SourceReference{Name: "app-artifact-2"},
	})
	if err := c.Update(context.Background(), ss); err != nil {
		t.Fatalf("add second stage: %v", err)
	}

	dirFirst := writeSourceTree(t, map[string]string{
		"cm.yaml": configMapManifest(ns, "from-first", map[string]string{"k": "v"}),
	})
	dirSecond := writeSourceTree(t, map[string]string{
		"cm.yaml": configMapManifest(ns, "from-second", map[string]string{"k": "v"}),
	})

	_, stderr, code := runCLI(t, cfg, "apply", "app", "-n", ns,
		"--source-dir", "first="+dirFirst, "--source-dir", "second="+dirSecond, "--stage", "first")
	if code != exitOK {
		t.Fatalf("apply --stage exit = %d (stderr=%s)", code, stderr)
	}

	if !cmExists(t, c, ns, "from-first") {
		t.Error("first stage's object should have been applied")
	}
	if cmExists(t, c, ns, "from-second") {
		t.Error("filtered-out stage's object must not be applied")
	}
}

// TestApply_Wait applies with --wait; a ConfigMap is immediately ready, so the
// readiness wait returns without error.
func TestApply_Wait(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "applywait")
	makeStageSet(t, c, ns, "app")
	dir := writeSourceTree(t, map[string]string{
		"cm.yaml": configMapManifest(ns, "settings", map[string]string{"k": "v"}),
	})

	_, stderr, code := runCLI(t, cfg, "apply", "app", "-n", ns, "--source-dir", dir, "--wait", "--timeout", "30s")
	if code != exitOK {
		t.Fatalf("apply --wait exit = %d (stderr=%s)", code, stderr)
	}
	if !cmExists(t, c, ns, "settings") {
		t.Error("ConfigMap should have been applied")
	}
}

// cmExists reports whether a ConfigMap exists in the namespace.
func cmExists(t *testing.T, c client.Client, ns, name string) bool {
	t.Helper()
	var cm corev1.ConfigMap
	err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &cm)
	if err == nil {
		return true
	}
	if apierrors.IsNotFound(err) {
		return false
	}
	t.Fatalf("get ConfigMap %s: %v", name, err)
	return false
}
