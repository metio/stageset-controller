// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package preview

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := stagesv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func stageSet(stageNames ...string) *stagesv1.StageSet {
	ss := &stagesv1.StageSet{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "app"}}
	for _, n := range stageNames {
		ss.Spec.Stages = append(ss.Spec.Stages, stagesv1.Stage{Name: n})
	}
	return ss
}

func TestSelectStages_AllWhenEmpty(t *testing.T) {
	got, err := SelectStages(stageSet("a", "b"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 stages, got %d", len(got))
	}
}

func TestSelectStages_SubsetInSpecOrder(t *testing.T) {
	got, err := SelectStages(stageSet("a", "b", "c"), []string{"c", "a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "a" || got[1].Name != "c" {
		t.Fatalf("want [a c] in spec order, got %v", names(got))
	}
}

func TestSelectStages_UnknownIsError(t *testing.T) {
	_, err := SelectStages(stageSet("a"), []string{"ghost"})
	if err == nil {
		t.Fatal("want error for unknown stage")
	}
}

func names(stages []stagesv1.Stage) []string {
	out := make([]string, len(stages))
	for i, s := range stages {
		out[i] = s.Name
	}
	return out
}

func TestReadDirFiles(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "a.yaml"), "x")
	mustWrite(t, filepath.Join(dir, "sub", "b.yaml"), "y")

	files, err := readDirFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	if files["a.yaml"] != "x" || files["sub/b.yaml"] != "y" {
		t.Fatalf("unexpected files: %v", files)
	}
}

func TestReadDirFiles_EmptyIsError(t *testing.T) {
	if _, err := readDirFiles(t.TempDir()); err == nil {
		t.Fatal("want error for empty dir")
	}
}

func TestResolvePostBuildVars(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "cfg"},
		Data:       map[string]string{"REGION": "eu", "TIER": "free"},
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "sec"},
		Data:       map[string][]byte{"TOKEN": []byte("abc")},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(cm, sec).Build()

	pb := &stagesv1.PostBuild{
		Substitute: map[string]string{"TIER": "premium"}, // inline wins over ConfigMap
		SubstituteFrom: []stagesv1.SubstituteReference{
			{Kind: "ConfigMap", Name: "cfg"},
			{Kind: "Secret", Name: "sec"},
			{Kind: "ConfigMap", Name: "missing", Optional: true},
		},
	}
	vars, err := resolvePostBuildVars(context.Background(), c, "ns", pb)
	if err != nil {
		t.Fatal(err)
	}
	if vars["REGION"] != "eu" || vars["TOKEN"] != "abc" || vars["TIER"] != "premium" {
		t.Fatalf("unexpected vars: %v", vars)
	}
}

func TestResolvePostBuildVars_RequiredMissingIsError(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	pb := &stagesv1.PostBuild{SubstituteFrom: []stagesv1.SubstituteReference{{Kind: "ConfigMap", Name: "gone"}}}
	if _, err := resolvePostBuildVars(context.Background(), c, "ns", pb); err == nil {
		t.Fatal("want error for missing required ConfigMap")
	}
}

func TestRenderStage_FromSourceDir(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "cm.yaml"), `apiVersion: v1
kind: ConfigMap
metadata:
  name: settings
data:
  greeting: hello
`)

	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	engine := NewEngine(c, false)
	engine.SourceDirs = map[string]string{"": dir}

	ss := stageSet("only")
	render, err := engine.RenderStage(context.Background(), ss, &ss.Spec.Stages[0])
	if err != nil {
		t.Fatalf("RenderStage: %v", err)
	}
	if !render.Local {
		t.Error("expected Local=true for --source-dir render")
	}
	if len(render.Objects) != 1 || render.Objects[0].GetName() != "settings" {
		t.Fatalf("unexpected render objects: %v", render.Objects)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
