// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	fluxmeta "github.com/fluxcd/pkg/apis/meta"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/artifact"
)

// TestReconcile_GateUnevaluable_PersistsLedgers pins that a gate-unevaluable
// exit persists everything the pass completed BEFORE the gate read failed: the
// migration ledger (a destructive delete must never re-run), the stage action
// ledger, and the handled force-reconcile token (an unhandled token would clear
// the ledger again next pass — the same re-run through another door). Without
// the persistence, every backoff retry re-executes the completed work.
func TestReconcile_GateUnevaluable_PersistsLedgers(t *testing.T) {
	cfg := envtestConfig(t)
	scheme := testScheme(t)
	base, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("NewWithWatch: %v", err)
	}
	ns := newNamespace(t, base)
	servedArtifact(t, base, ns, "ea", "", map[string]string{"cm.yaml": cmValManifest(ns, "gated-obj", "v1")})

	// Victims: the inline migration deletes migration-victim; the stage's
	// pre-action deletes action-victim. Both deletes are the observable proof of
	// (re-)execution.
	mkVictim := func(name string) {
		t.Helper()
		if err := base.Create(context.Background(), &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}
	mkVictim("migration-victim")
	mkVictim("action-victim")

	var failPods bool
	gateReaderClient := interceptor.NewClient(base, interceptor.Funcs{
		List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*corev1.PodList); ok && failPods {
				return apierrors.NewServiceUnavailable("transient apiserver hiccup")
			}
			return cl.List(ctx, list, opts...)
		},
	})

	deleteAction := func(name, target string) stagesv1.Action {
		return stagesv1.Action{
			Name: name,
			Delete: &stagesv1.DeleteAction{Target: fluxmeta.NamespacedObjectKindReference{
				APIVersion: "v1", Kind: "ConfigMap", Name: target, Namespace: ns,
			}},
		}
	}

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "gate-ledger"},
		Spec: stagesv1.StageSetSpec{
			Interval: metav1.Duration{Duration: time.Minute},
			Version:  &stagesv1.VersionSource{Value: "1.0.0"},
			Migrations: []stagesv1.Migration{{
				Name:    "drop-legacy",
				To:      "2.0.0",
				Stage:   "stage-a",
				Actions: []stagesv1.Action{deleteAction("delete-legacy", "migration-victim")},
			}},
			Stages: []stagesv1.Stage{{
				Name:      "stage-a",
				SourceRef: stagesv1.SourceReference{Name: "ea"},
				Actions:   &stagesv1.StageActions{Pre: []stagesv1.Action{deleteAction("drop-action-victim", "action-victim")}},
				Promotion: &stagesv1.StagePromotion{RestartGate: &stagesv1.RestartGate{
					Checks: []stagesv1.RestartCheck{{
						Name:        "api",
						Selector:    metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}},
						MaxRestarts: 5,
					}},
				}},
			}},
		},
	}
	if err := base.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	newReconciler := func() *StageSetReconciler {
		return &StageSetReconciler{
			Client:     base,
			APIReader:  gateReaderClient,
			RESTMapper: base.RESTMapper(),
			Fetcher:    &artifact.Fetcher{HTTPClient: http.DefaultClient, URLValidator: artifact.PermissiveHTTPURL, IPValidator: artifact.PermissiveIP},
		}
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "gate-ledger"}}

	// Pass 1 (gate healthy): baseline at 1.0.0. The pre-action runs (deletes
	// action-victim), no migration runs at baseline, gate promotes, Synced.
	if _, err := driveReconcile(newReconciler(), req); err != nil {
		t.Fatalf("baseline reconcile: %v", err)
	}
	if got := readyReason(getStageSet(t, base, ns, "gate-ledger")); got != ReasonReady {
		t.Fatalf("baseline Ready reason = %q, want %q", got, ReasonReady)
	}
	if cmExists(t, base, ns, "migration-victim") == false {
		t.Fatal("baseline must not run migrations")
	}

	// Recreate the action victim, bump the version (arms the migration), and
	// force stage-a (clears its action ledger so the pre-action re-runs).
	mkVictim("action-victim")
	cur := getStageSet(t, base, ns, "gate-ledger")
	cur.Spec.Version = &stagesv1.VersionSource{Value: "2.0.0"}
	if cur.Annotations == nil {
		cur.Annotations = map[string]string{}
	}
	cur.Annotations[reconcileStageAnnotation] = "stage-a@force1"
	if err := base.Update(context.Background(), cur); err != nil {
		t.Fatalf("bump version + force: %v", err)
	}

	// Pass 2 (gate read FAILS): the migration runs (deletes migration-victim),
	// the forced pre-action runs (deletes action-victim), the stage applies —
	// then the restart-gate read fails and the reconcile exits unevaluable.
	failPods = true
	if _, rerr := newReconciler().Reconcile(context.Background(), req); rerr == nil {
		t.Fatal("the unevaluable gate must surface an error for backoff")
	}
	if cmExists(t, base, ns, "migration-victim") {
		t.Fatal("the migration should have run before the gate read failed")
	}
	if cmExists(t, base, ns, "action-victim") {
		t.Fatal("the forced pre-action should have run before the gate read failed")
	}

	// THE FIX: the pass's ledgers persist despite the unevaluable exit.
	mid := getStageSet(t, base, ns, "gate-ledger")
	migDone := slices.ContainsFunc(mid.Status.ExecutedMigrations, func(k string) bool {
		return strings.HasPrefix(k, "drop-legacy")
	})
	if !migDone {
		t.Fatalf("the completed migration must persist in status.executedMigrations across a gate-unevaluable exit, got %v", mid.Status.ExecutedMigrations)
	}
	sa := stageStatusFor(mid, "stage-a")
	if sa == nil {
		t.Fatal("stage-a's in-flight status entry must persist across a gate-unevaluable exit")
	}
	if !slices.Contains(sa.ExecutedActions, "drop-action-victim") {
		t.Fatalf("stage-a's action ledger must persist, got %v", sa.ExecutedActions)
	}
	if sa.LastHandledReconcileAt != "force1" {
		t.Fatalf("the force token must be recorded handled (or the next pass clears the ledger again), got %q", sa.LastHandledReconcileAt)
	}

	// Recreate both victims; pass 3 retries with the gate STILL failing. The
	// persisted ledgers must prevent any re-execution: both victims survive.
	mkVictim("migration-victim")
	mkVictim("action-victim")
	if _, rerr := newReconciler().Reconcile(context.Background(), req); rerr == nil {
		t.Fatal("the gate is still unevaluable; pass 3 must error for backoff")
	}
	if !cmExists(t, base, ns, "migration-victim") {
		t.Fatal("retry re-ran the destructive migration despite the persisted ledger")
	}
	if !cmExists(t, base, ns, "action-victim") {
		t.Fatal("retry re-ran the stage pre-action despite the persisted ledger and handled force token")
	}

	// Gate recovers: the transition completes, version advances, ledgers clear.
	failPods = false
	if _, err := driveReconcile(newReconciler(), req); err != nil {
		t.Fatalf("recovery reconcile: %v", err)
	}
	final := getStageSet(t, base, ns, "gate-ledger")
	if final.Status.Version != "2.0.0" {
		t.Fatalf("version should advance once the gate is evaluable again, got %q", final.Status.Version)
	}
	if !cmExists(t, base, ns, "migration-victim") {
		t.Fatal("the recovered pass must still honor the migration ledger (no re-run)")
	}
}
