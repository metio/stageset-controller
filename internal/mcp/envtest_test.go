/*
 * SPDX-FileCopyrightText: The stageset-controller Authors
 * SPDX-License-Identifier: 0BSD
 */

package mcp

import (
	"context"
	"os"
	"path/filepath"
	goruntime "runtime"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

// envtestClient boots a real kube-apiserver + etcd with the StageSet CRDs
// installed and returns a client wired to it. It skips when KUBEBUILDER_ASSETS
// is unset, matching the controller package's envtest convention.
func envtestClient(t *testing.T) client.Client {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("envtest assets unavailable (set KUBEBUILDER_ASSETS or run inside the dev shell)")
	}
	_, here, _, ok := goruntime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test file via runtime.Caller")
	}
	crdDir := filepath.Join(filepath.Dir(here), "..", "..", "config", "crd")

	env := &envtest.Environment{CRDDirectoryPaths: []string{crdDir}, ErrorIfCRDPathMissing: true}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("envtest start: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	scheme := runtime.NewScheme()
	if err := stagesv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	return c
}

// TestEnvtest_ToolsAgainstRealAPIServer exercises the read and write tools end
// to end against a real apiserver: a real CRD schema, real List/Get, and real
// MergeFrom patches — the integration layer the fake client can't fully model.
func TestEnvtest_ToolsAgainstRealAPIServer(t *testing.T) {
	c := envtestClient(t)
	ctx := context.Background()

	// The "default" namespace exists in a fresh envtest; create a schema-valid
	// StageSet there (stages requires MinItems=1 with a SourceRef).
	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "envtest-demo"},
		Spec: stagesv1.StageSetSpec{
			Interval: metav1.Duration{Duration: time.Minute},
			Stages: []stagesv1.Stage{{
				Name:      "s",
				SourceRef: stagesv1.SourceReference{Name: "x"},
				Actions:   &stagesv1.StageActions{Pre: []stagesv1.Action{{Name: "ok", Wait: &stagesv1.WaitAction{}}}},
			}},
		},
	}
	if err := c.Create(ctx, ss); err != nil {
		t.Fatalf("create stageset: %v", err)
	}

	cfg := Config{KubeClient: c, RunbookBaseURL: testRunbookBase, AllowMutations: true}

	_, listOut, err := cfg.listStageSetsHandler(ctx, nil, listStageSetsInput{Namespace: "default"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listOut.StageSets) != 1 || listOut.StageSets[0].Name != "envtest-demo" {
		t.Fatalf("list returned %+v", listOut.StageSets)
	}

	_, getOut, err := cfg.getStageSetHandler(ctx, nil, getStageSetInput{Namespace: "default", Name: "envtest-demo"})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if getOut.Ready != "Unknown" || getOut.Suspended {
		t.Fatalf("get returned %+v", getOut)
	}

	if _, _, err := cfg.suspendStageSetHandler(ctx, nil, mutateInput{Namespace: "default", Name: "envtest-demo"}); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	var after stagesv1.StageSet
	if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "envtest-demo"}, &after); err != nil {
		t.Fatalf("re-get: %v", err)
	}
	if !after.Spec.Suspend {
		t.Fatal("suspend did not persist on the real apiserver")
	}

	if _, _, err := cfg.reconcileStageSetHandler(ctx, nil, mutateInput{Namespace: "default", Name: "envtest-demo"}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "envtest-demo"}, &after); err != nil {
		t.Fatalf("re-get: %v", err)
	}
	if after.Annotations["reconcile.fluxcd.io/requestedAt"] == "" {
		t.Fatal("reconcile annotation did not persist")
	}
}
