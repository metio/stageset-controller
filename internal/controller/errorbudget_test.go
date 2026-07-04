// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/artifact"
	"github.com/metio/stageset-controller/internal/metrics"
	"github.com/metio/stageset-controller/internal/metricsource"
	"github.com/metio/stageset-controller/internal/window"
)

// fakeQuerier returns a scriptable scalar/error for the metric gates, counting
// calls so a test can assert a gate short-circuited before querying.
type fakeQuerier struct {
	value float64
	err   error
	calls int
}

func (f *fakeQuerier) Query(context.Context, string, stagesv1.MetricSource) (float64, error) {
	f.calls++
	return f.value, f.err
}

// reconcileWithQuerier drives one reconcile with a fake metric querier wired in.
func reconcileWithQuerier(t *testing.T, c client.Client, ss *stagesv1.StageSet, now time.Time, q metricsource.Querier) {
	t.Helper()
	r := &StageSetReconciler{
		Client:            c,
		RESTMapper:        c.RESTMapper(),
		Fetcher:           &artifact.Fetcher{HTTPClient: http.DefaultClient, URLValidator: artifact.PermissiveHTTPURL, IPValidator: artifact.PermissiveIP},
		Now:               func() time.Time { return now },
		MetricQuerier:     q,
		MetricIPValidator: metricsource.PermissiveIP,
	}
	_, _ = driveReconcile(r, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ss)})
}

func promSource() stagesv1.MetricSource {
	return stagesv1.MetricSource{Prometheus: &stagesv1.PrometheusSource{
		Address: "http://prometheus.monitoring:9090",
		Query:   "slo:period_error_budget_remaining:ratio",
	}}
}

// Budget below freezeThreshold holds the first rollout: nothing applies, the
// StageSet reports BudgetExhausted, and status.budgetFreeze records the value.
func TestReconcile_ErrorBudget_FreezesWhenExhausted(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	servedArtifact(t, c, ns, "ea", "", map[string]string{"cm.yaml": configMapManifest(ns, "budgeted")})

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "frozen"},
		Spec: stagesv1.StageSetSpec{
			Interval:    metav1.Duration{Duration: time.Minute},
			ErrorBudget: &stagesv1.ErrorBudget{Source: promSource(), FreezeThreshold: "0.1"},
			Stages:      []stagesv1.Stage{{Name: "stage-a", SourceRef: stagesv1.SourceReference{Name: "ea"}}},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	reconcileWithQuerier(t, c, ss, tm("2026-01-01T00:00:00Z").Time, &fakeQuerier{value: 0.05})

	if cmExists(t, c, ns, "budgeted") {
		t.Fatal("an exhausted error budget must hold the rollout")
	}
	got := getStageSet(t, c, ns, "frozen")
	if readyReason(got) != ReasonBudgetExhausted {
		t.Fatalf("Ready reason = %q, want %q", readyReason(got), ReasonBudgetExhausted)
	}
	if got.Status.BudgetFreeze == nil || got.Status.BudgetFreeze.Remaining != "0.05" {
		t.Fatalf("status.budgetFreeze = %+v, want Remaining 0.05", got.Status.BudgetFreeze)
	}
	if v := testutil.ToFloat64(metrics.BudgetFrozen.WithLabelValues(ns, "frozen")); v != 1 {
		t.Errorf("budget_frozen gauge = %v, want 1", v)
	}
}

// A healthy budget lets the rollout through and records no freeze.
func TestReconcile_ErrorBudget_AllowsWhenHealthy(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	servedArtifact(t, c, ns, "ea", "", map[string]string{"cm.yaml": configMapManifest(ns, "healthy")})

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "ok"},
		Spec: stagesv1.StageSetSpec{
			Interval:    metav1.Duration{Duration: time.Minute},
			ErrorBudget: &stagesv1.ErrorBudget{Source: promSource(), FreezeThreshold: "0.1"},
			Stages:      []stagesv1.Stage{{Name: "stage-a", SourceRef: stagesv1.SourceReference{Name: "ea"}}},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	reconcileWithQuerier(t, c, ss, tm("2026-01-01T00:00:00Z").Time, &fakeQuerier{value: 0.9})

	if !cmExists(t, c, ns, "healthy") {
		t.Fatal("a healthy budget should permit the rollout")
	}
	got := getStageSet(t, c, ns, "ok")
	if readyReason(got) != ReasonReady {
		t.Fatalf("Ready reason = %q, want %q", readyReason(got), ReasonReady)
	}
	if got.Status.BudgetFreeze != nil {
		t.Fatalf("status.budgetFreeze should be cleared, got %+v", got.Status.BudgetFreeze)
	}
}

// A closed update window holds the rollout even when the budget is healthy — the
// two gates are combined under AND — and the budget source is not even queried.
func TestReconcile_ErrorBudget_RespectsClosedUpdateWindow(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	servedArtifact(t, c, ns, "ea", "", map[string]string{"cm.yaml": configMapManifest(ns, "windowed")})

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "both-gates"},
		Spec: stagesv1.StageSetSpec{
			Interval:      metav1.Duration{Duration: time.Minute},
			UpdateWindows: []stagesv1.UpdateWindow{{Type: window.TypeDeny, From: tm("2020-01-01T00:00:00Z"), To: tm("2030-01-01T00:00:00Z")}},
			ErrorBudget:   &stagesv1.ErrorBudget{Source: promSource(), FreezeThreshold: "0.1"},
			Stages:        []stagesv1.Stage{{Name: "stage-a", SourceRef: stagesv1.SourceReference{Name: "ea"}}},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	q := &fakeQuerier{value: 0.9} // budget healthy
	reconcileWithQuerier(t, c, ss, tm("2025-06-01T00:00:00Z").Time, q)

	if cmExists(t, c, ns, "windowed") {
		t.Fatal("a closed update window must hold the rollout even with a healthy budget")
	}
	if r := readyReason(getStageSet(t, c, ns, "both-gates")); r != ReasonUpdateDeferred {
		t.Fatalf("Ready reason = %q, want %q (window gates first)", r, ReasonUpdateDeferred)
	}
	if q.calls != 0 {
		t.Errorf("budget source queried %d times; a closed window should short-circuit before the budget query", q.calls)
	}
}

// An exhausted budget holds the rollout even inside an open update window —
// proving the budget gate is enforced in addition to (not instead of) windows.
func TestReconcile_ErrorBudget_FreezesInsideOpenWindow(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	servedArtifact(t, c, ns, "ea", "", map[string]string{"cm.yaml": configMapManifest(ns, "openwin")})

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "open-but-frozen"},
		Spec: stagesv1.StageSetSpec{
			Interval:      metav1.Duration{Duration: time.Minute},
			UpdateWindows: []stagesv1.UpdateWindow{{Type: window.TypeAllow, From: tm("2020-01-01T00:00:00Z"), To: tm("2030-01-01T00:00:00Z")}},
			ErrorBudget:   &stagesv1.ErrorBudget{Source: promSource(), FreezeThreshold: "0.1"},
			Stages:        []stagesv1.Stage{{Name: "stage-a", SourceRef: stagesv1.SourceReference{Name: "ea"}}},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	reconcileWithQuerier(t, c, ss, tm("2025-06-01T00:00:00Z").Time, &fakeQuerier{value: 0.0})

	if cmExists(t, c, ns, "openwin") {
		t.Fatal("an exhausted budget must hold the rollout even inside an open window")
	}
	if r := readyReason(getStageSet(t, c, ns, "open-but-frozen")); r != ReasonBudgetExhausted {
		t.Fatalf("Ready reason = %q, want %q", r, ReasonBudgetExhausted)
	}
}

// Hysteresis: once frozen, the rollout stays frozen until the budget recovers to
// resumeThreshold, not merely back above freezeThreshold.
func TestReconcile_ErrorBudget_Hysteresis(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	servedArtifact(t, c, ns, "ea", "", map[string]string{"cm.yaml": configMapManifest(ns, "hyst")})

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "hysteresis"},
		Spec: stagesv1.StageSetSpec{
			Interval:    metav1.Duration{Duration: time.Minute},
			ErrorBudget: &stagesv1.ErrorBudget{Source: promSource(), FreezeThreshold: "0", ResumeThreshold: "0.05"},
			Stages:      []stagesv1.Stage{{Name: "stage-a", SourceRef: stagesv1.SourceReference{Name: "ea"}}},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	q := &fakeQuerier{}
	start := tm("2026-01-01T00:00:00Z").Time

	// Overspent → freeze.
	q.value = -0.01
	reconcileWithQuerier(t, c, ss, start, q)
	if cmExists(t, c, ns, "hyst") {
		t.Fatal("overspent budget should freeze")
	}

	// Back above freeze (0) but below resume (0.05) → stays frozen (hysteresis).
	q.value = 0.02
	reconcileWithQuerier(t, c, getStageSet(t, c, ns, "hysteresis"), start.Add(time.Minute), q)
	if cmExists(t, c, ns, "hyst") {
		t.Fatal("budget below resumeThreshold should stay frozen (hysteresis)")
	}

	// Recovered to resumeThreshold → resume.
	q.value = 0.06
	reconcileWithQuerier(t, c, getStageSet(t, c, ns, "hysteresis"), start.Add(2*time.Minute), q)
	if !cmExists(t, c, ns, "hyst") {
		t.Fatal("budget at resumeThreshold should resume the rollout")
	}
}

// dryRun records what would freeze but does not hold the rollout.
func TestReconcile_ErrorBudget_DryRunRecordsButProceeds(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	servedArtifact(t, c, ns, "ea", "", map[string]string{"cm.yaml": configMapManifest(ns, "dry")})

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "dryrun"},
		Spec: stagesv1.StageSetSpec{
			Interval:    metav1.Duration{Duration: time.Minute},
			ErrorBudget: &stagesv1.ErrorBudget{Source: promSource(), FreezeThreshold: "0.1", DryRun: true},
			Stages:      []stagesv1.Stage{{Name: "stage-a", SourceRef: stagesv1.SourceReference{Name: "ea"}}},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	reconcileWithQuerier(t, c, ss, tm("2026-01-01T00:00:00Z").Time, &fakeQuerier{value: 0.01})

	if !cmExists(t, c, ns, "dry") {
		t.Fatal("dryRun must not hold the rollout")
	}
	got := getStageSet(t, c, ns, "dryrun")
	if got.Status.BudgetFreeze == nil || !got.Status.BudgetFreeze.DryRun {
		t.Fatalf("dryRun should record a would-freeze on status.budgetFreeze, got %+v", got.Status.BudgetFreeze)
	}
	if readyReason(got) != ReasonReady {
		t.Fatalf("dryRun Ready reason = %q, want %q", readyReason(got), ReasonReady)
	}
}

// A source error with onSourceError=Allow (default) proceeds and counts the error.
func TestReconcile_ErrorBudget_SourceErrorAllowProceeds(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	servedArtifact(t, c, ns, "ea", "", map[string]string{"cm.yaml": configMapManifest(ns, "srcerr-allow")})

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "src-allow"},
		Spec: stagesv1.StageSetSpec{
			Interval:    metav1.Duration{Duration: time.Minute},
			ErrorBudget: &stagesv1.ErrorBudget{Source: promSource(), FreezeThreshold: "0.1"}, // onSourceError defaults Allow
			Stages:      []stagesv1.Stage{{Name: "stage-a", SourceRef: stagesv1.SourceReference{Name: "ea"}}},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	before := testutil.ToFloat64(metrics.MetricSourceErrorsTotal.WithLabelValues(ns, "src-allow"))
	reconcileWithQuerier(t, c, ss, tm("2026-01-01T00:00:00Z").Time, &fakeQuerier{err: errors.New("prometheus down")})

	if !cmExists(t, c, ns, "srcerr-allow") {
		t.Fatal("onSourceError=Allow should proceed when the source is down")
	}
	if delta := testutil.ToFloat64(metrics.MetricSourceErrorsTotal.WithLabelValues(ns, "src-allow")) - before; delta != 1 {
		t.Errorf("metric_source_errors delta = %v, want 1", delta)
	}
}

// A source error with onSourceError=Hold blocks the rollout.
func TestReconcile_ErrorBudget_SourceErrorHoldBlocks(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	servedArtifact(t, c, ns, "ea", "", map[string]string{"cm.yaml": configMapManifest(ns, "srcerr-hold")})

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "src-hold"},
		Spec: stagesv1.StageSetSpec{
			Interval:    metav1.Duration{Duration: time.Minute},
			ErrorBudget: &stagesv1.ErrorBudget{Source: promSource(), FreezeThreshold: "0.1", OnSourceError: "Hold"},
			Stages:      []stagesv1.Stage{{Name: "stage-a", SourceRef: stagesv1.SourceReference{Name: "ea"}}},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	reconcileWithQuerier(t, c, ss, tm("2026-01-01T00:00:00Z").Time, &fakeQuerier{err: errors.New("prometheus down")})

	if cmExists(t, c, ns, "srcerr-hold") {
		t.Fatal("onSourceError=Hold should block when the source is down")
	}
	if r := readyReason(getStageSet(t, c, ns, "src-hold")); r != ReasonBudgetSourceUnavailable {
		t.Fatalf("Ready reason = %q, want %q", r, ReasonBudgetSourceUnavailable)
	}
}

// The budget-override annotation ships a held rollout once.
func TestReconcile_ErrorBudget_OverrideForcesApply(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	servedArtifact(t, c, ns, "ea", "", map[string]string{"cm.yaml": configMapManifest(ns, "overridden")})

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   ns,
			Name:        "budget-override",
			Annotations: map[string]string{budgetOverrideAnnotation: "tok-1"},
		},
		Spec: stagesv1.StageSetSpec{
			Interval:    metav1.Duration{Duration: time.Minute},
			ErrorBudget: &stagesv1.ErrorBudget{Source: promSource(), FreezeThreshold: "0.1"},
			Stages:      []stagesv1.Stage{{Name: "stage-a", SourceRef: stagesv1.SourceReference{Name: "ea"}}},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	q := &fakeQuerier{value: 0.0} // exhausted, but override wins
	reconcileWithQuerier(t, c, ss, tm("2026-01-01T00:00:00Z").Time, q)

	if !cmExists(t, c, ns, "overridden") {
		t.Fatal("budget-override should ship the held rollout")
	}
	got := getStageSet(t, c, ns, "budget-override")
	if got.Status.LastHandledBudgetOverride != "tok-1" {
		t.Fatalf("lastHandledBudgetOverride = %q, want tok-1", got.Status.LastHandledBudgetOverride)
	}
	if q.calls != 0 {
		t.Errorf("override should ship without querying the source, got %d calls", q.calls)
	}
}

// An already-deployed StageSet whose new revision is frozen stays Ready=True; the
// old objects remain and the new revision is not applied.
func TestReconcile_ErrorBudget_DeployedStaysReadyWhenFrozen(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	servedArtifact(t, c, ns, "ea", "", map[string]string{"cm.yaml": cmValManifest(ns, "rolling-budget", "v1")})

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "deployed-frozen"},
		Spec: stagesv1.StageSetSpec{
			Interval:    metav1.Duration{Duration: time.Minute},
			ErrorBudget: &stagesv1.ErrorBudget{Source: promSource(), FreezeThreshold: "0.1"},
			Stages:      []stagesv1.Stage{{Name: "stage-a", SourceRef: stagesv1.SourceReference{Name: "ea"}}},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	// Healthy budget → initial deploy applies v1.
	reconcileWithQuerier(t, c, ss, tm("2026-01-01T00:00:00Z").Time, &fakeQuerier{value: 0.9})
	if cmDataKey(t, c, ns, "rolling-budget") != "v1" {
		t.Fatal("initial deploy should apply v1")
	}

	// New revision v2, but the budget is now exhausted.
	repointArtifact(t, c, ns, "ea", map[string]string{"cm.yaml": cmValManifest(ns, "rolling-budget", "v2")})
	reconcileWithQuerier(t, c, getStageSet(t, c, ns, "deployed-frozen"), tm("2026-01-01T00:05:00Z").Time, &fakeQuerier{value: 0.0})

	got := getStageSet(t, c, ns, "deployed-frozen")
	if readyReason(got) != ReasonReady {
		t.Fatalf("a deployed StageSet with a frozen update should stay Ready, reason = %q", readyReason(got))
	}
	if got.Status.BudgetFreeze == nil {
		t.Fatal("status.budgetFreeze should record the freeze")
	}
	if v := cmDataKey(t, c, ns, "rolling-budget"); v != "v1" {
		t.Fatalf("the frozen update must not apply; want v1, got %q", v)
	}
}

// A per-stage errorBudget freezes a NEW revision out of that stage while its
// budget is exhausted; earlier stages still apply, and it resumes on recovery.
func TestReconcile_StageErrorBudget_FreezesEntryThenResumes(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	servedArtifact(t, c, ns, "ea-a", "", map[string]string{"cm.yaml": configMapManifest(ns, "stg-a")})
	servedArtifact(t, c, ns, "ea-b", "", map[string]string{"cm.yaml": configMapManifest(ns, "stg-b")})

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "stage-budget"},
		Spec: stagesv1.StageSetSpec{
			Interval: metav1.Duration{Duration: time.Minute},
			Stages: []stagesv1.Stage{
				{Name: "stage-a", SourceRef: stagesv1.SourceReference{Name: "ea-a"}},
				{Name: "stage-b", SourceRef: stagesv1.SourceReference{Name: "ea-b"}, ErrorBudget: &stagesv1.ErrorBudget{Source: promSource(), FreezeThreshold: "0.1"}},
			},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	q := &fakeQuerier{value: 0.0} // stage-b budget exhausted
	start := tm("2026-01-01T00:00:00Z").Time
	reconcileWithQuerier(t, c, ss, start, q)

	if !cmExists(t, c, ns, "stg-a") {
		t.Fatal("stage-a (no budget) should apply")
	}
	if cmExists(t, c, ns, "stg-b") {
		t.Fatal("stage-b must be held out by its exhausted error budget")
	}
	got := getStageSet(t, c, ns, "stage-budget")
	if readyReason(got) != ReasonBudgetExhausted {
		t.Fatalf("Ready reason = %q, want %q", readyReason(got), ReasonBudgetExhausted)
	}
	var stageB *stagesv1.StageStatus
	for i := range got.Status.Stages {
		if got.Status.Stages[i].Name == "stage-b" {
			stageB = &got.Status.Stages[i]
		}
	}
	if stageB == nil || stageB.BudgetFreeze == nil {
		t.Fatalf("stage-b status should carry a budgetFreeze: %+v", got.Status.Stages)
	}
	if v := testutil.ToFloat64(metrics.StageBudgetFrozen.WithLabelValues(ns, "stage-budget", "stage-b")); v != 1 {
		t.Errorf("stage_budget_frozen gauge = %v, want 1", v)
	}

	// Budget recovers → stage-b applies.
	q.value = 0.9
	reconcileWithQuerier(t, c, getStageSet(t, c, ns, "stage-budget"), start.Add(time.Minute), q)
	if !cmExists(t, c, ns, "stg-b") {
		t.Fatal("stage-b should apply once its budget recovers")
	}
	if r := readyReason(getStageSet(t, c, ns, "stage-budget")); r != ReasonReady {
		t.Fatalf("Ready reason after recovery = %q, want %q", r, ReasonReady)
	}
}

// A per-stage freeze that is later re-evaluated under an unreadable source with
// onSourceError=Allow proceeds, and must not leave the per-stage frozen gauge
// stuck at 1 — the gauge reflects an active hold, which the proceeding reconcile
// no longer has.
func TestReconcile_StageErrorBudget_SourceErrorAllowClearsFrozenGauge(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	servedArtifact(t, c, ns, "ea-a", "", map[string]string{"cm.yaml": configMapManifest(ns, "clr-a")})
	servedArtifact(t, c, ns, "ea-b", "", map[string]string{"cm.yaml": configMapManifest(ns, "clr-b")})

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "stage-budget-clear"},
		Spec: stagesv1.StageSetSpec{
			Interval: metav1.Duration{Duration: time.Minute},
			Stages: []stagesv1.Stage{
				{Name: "stage-a", SourceRef: stagesv1.SourceReference{Name: "ea-a"}},
				// onSourceError unset defaults to Allow.
				{Name: "stage-b", SourceRef: stagesv1.SourceReference{Name: "ea-b"}, ErrorBudget: &stagesv1.ErrorBudget{Source: promSource(), FreezeThreshold: "0.1"}},
			},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}

	// Reconcile 1: budget exhausted → stage-b frozen out, gauge set to 1.
	start := tm("2026-01-01T00:00:00Z").Time
	reconcileWithQuerier(t, c, ss, start, &fakeQuerier{value: 0.0})
	if cmExists(t, c, ns, "clr-b") {
		t.Fatal("stage-b must be held out by its exhausted budget on the first reconcile")
	}
	if v := testutil.ToFloat64(metrics.StageBudgetFrozen.WithLabelValues(ns, "stage-budget-clear", "stage-b")); v != 1 {
		t.Fatalf("precondition: stage_budget_frozen gauge = %v, want 1", v)
	}

	// Reconcile 2: source now unreadable; onSourceError=Allow proceeds and applies
	// stage-b. The gauge must fall back to 0 — the stage is no longer held.
	reconcileWithQuerier(t, c, getStageSet(t, c, ns, "stage-budget-clear"), start.Add(time.Minute), &fakeQuerier{err: errors.New("prometheus unreachable")})
	if !cmExists(t, c, ns, "clr-b") {
		t.Fatal("stage-b should apply once the budget source is unreadable under onSourceError=Allow")
	}
	if v := testutil.ToFloat64(metrics.StageBudgetFrozen.WithLabelValues(ns, "stage-budget-clear", "stage-b")); v != 0 {
		t.Errorf("stage_budget_frozen gauge = %v after proceeding under onSourceError=Allow, want 0 (stale freeze gauge)", v)
	}
}

// The budget-override break-glass ships a per-stage-frozen rollout once.
func TestReconcile_StageErrorBudget_OverrideShipsOnce(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	servedArtifact(t, c, ns, "ea-a", "", map[string]string{"cm.yaml": configMapManifest(ns, "ov-a")})
	servedArtifact(t, c, ns, "ea-b", "", map[string]string{"cm.yaml": configMapManifest(ns, "ov-b")})

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   ns,
			Name:        "stage-budget-ov",
			Annotations: map[string]string{budgetOverrideAnnotation: "tok-1"},
		},
		Spec: stagesv1.StageSetSpec{
			Interval: metav1.Duration{Duration: time.Minute},
			Stages: []stagesv1.Stage{
				{Name: "stage-a", SourceRef: stagesv1.SourceReference{Name: "ea-a"}},
				{Name: "stage-b", SourceRef: stagesv1.SourceReference{Name: "ea-b"}, ErrorBudget: &stagesv1.ErrorBudget{Source: promSource(), FreezeThreshold: "0.1"}},
			},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	q := &fakeQuerier{value: 0.0} // exhausted, but override wins
	reconcileWithQuerier(t, c, ss, tm("2026-01-01T00:00:00Z").Time, q)

	if !cmExists(t, c, ns, "ov-b") {
		t.Fatal("budget-override should ship stage-b despite the exhausted budget")
	}
	if q.calls != 0 {
		t.Errorf("override should skip the per-stage budget query, got %d calls", q.calls)
	}
}

func TestBudgetThresholds(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name              string
		eb                *stagesv1.ErrorBudget
		wantFreeze, wantR float64
		wantErr           bool
	}{
		{"resume defaults to freeze", &stagesv1.ErrorBudget{FreezeThreshold: "0.1"}, 0.1, 0.1, false},
		{"explicit resume", &stagesv1.ErrorBudget{FreezeThreshold: "0", ResumeThreshold: "0.05"}, 0, 0.05, false},
		{"resume below freeze rejected", &stagesv1.ErrorBudget{FreezeThreshold: "0.1", ResumeThreshold: "0.05"}, 0, 0, true},
		{"bad freeze", &stagesv1.ErrorBudget{FreezeThreshold: "x"}, 0, 0, true},
		{"bad resume", &stagesv1.ErrorBudget{FreezeThreshold: "0", ResumeThreshold: "y"}, 0, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, r, err := budgetThresholds(tc.eb)
			if tc.wantErr {
				if err == nil {
					t.Fatal("want error")
				}
				return
			}
			if err != nil || f != tc.wantFreeze || r != tc.wantR {
				t.Fatalf("got (%v,%v,%v), want (%v,%v,nil)", f, r, err, tc.wantFreeze, tc.wantR)
			}
		})
	}
}
