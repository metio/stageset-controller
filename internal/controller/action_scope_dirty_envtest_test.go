// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"net/http"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

// revisionFailStageSet has a single Revision-scoped post action that hits a
// failing endpoint — a stage that fails without any scope: Lifetime action.
func revisionFailStageSet(ns, name, endpoint string) *stagesv1.StageSet {
	return &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: stagesv1.StageSetSpec{
			Interval: metav1.Duration{Duration: time.Minute},
			Stages: []stagesv1.Stage{{
				Name:      "app",
				SourceRef: stagesv1.SourceReference{Name: "ea"},
				Actions: &stagesv1.StageActions{Post: []stagesv1.Action{
					{Name: "notify", Scope: stagesv1.ScopeRevision, HTTP: &stagesv1.HTTPAction{URL: endpoint}},
				}},
			}},
		},
	}
}

// A scope: Lifetime bootstrap that keeps failing escalates from StageFailed
// (which retries) to ActionDirty (which halts), so a destructive bootstrap stops
// auto-retrying against an uncertain state. A manual reconcile then clears the
// halt and retries once.
func TestActionScope_ActionDirtyEscalatesAndClears(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	var hits int32
	endpoint := countingServer(t, http.StatusInternalServerError, &hits)
	hosts := []string{actionHost(t, endpoint)}

	servedArtifact(t, c, ns, "ea", "", map[string]string{"cm.yaml": configMapManifest(ns, "obj")})
	ss := lifetimeStageSet(ns, "dirty", endpoint) // install-database hits a 500
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}

	var reason string
	for range maxActionFailures + 2 {
		_ = reconcileWith(t, c, ss, hosts) // the stage fails; the error drives backoff
		reason = readyReason(getStageSet(t, c, ns, "dirty"))
		if reason == ReasonActionDirty {
			break
		}
	}
	if reason != ReasonActionDirty {
		t.Fatalf("a repeatedly-failing scope: Lifetime bootstrap must escalate to ActionDirty; got %q", reason)
	}
	if got := getStageSet(t, c, ns, "dirty").Status.ActionFailureCount; got < maxActionFailures {
		t.Errorf("ActionFailureCount = %d, want >= %d", got, maxActionFailures)
	}

	// A manual reconcile resets the counter and retries once — back to the
	// retrying StageFailed state, not the halted ActionDirty one.
	stampAnnotation(t, c, ns, "dirty", "reconcile.fluxcd.io/requestedAt", "recover-1")
	_ = reconcileWith(t, c, ss, hosts)
	if r := readyReason(getStageSet(t, c, ns, "dirty")); r == ReasonActionDirty {
		t.Errorf("a manual reconcile must clear the ActionDirty halt and retry once; still %q", r)
	}
}

// A stage with no incomplete scope: Lifetime action never escalates to
// ActionDirty however often it fails — the halt is reserved for a bootstrap
// whose re-attempt could be destructive.
func TestActionScope_RevisionFailureNeverDirty(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	var hits int32
	endpoint := countingServer(t, http.StatusInternalServerError, &hits)
	hosts := []string{actionHost(t, endpoint)}

	servedArtifact(t, c, ns, "ea", "", map[string]string{"cm.yaml": configMapManifest(ns, "obj")})
	ss := revisionFailStageSet(ns, "revfail", endpoint)
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}

	for range maxActionFailures + 3 {
		_ = reconcileWith(t, c, ss, hosts)
		if r := readyReason(getStageSet(t, c, ns, "revfail")); r == ReasonActionDirty {
			t.Fatal("a non-Lifetime action failure must never escalate to ActionDirty")
		}
	}
	if r := readyReason(getStageSet(t, c, ns, "revfail")); r != ReasonStageFailed {
		t.Errorf("expected the stage to be failing (StageFailed); got %q", r)
	}
	if got := getStageSet(t, c, ns, "revfail").Status.ActionFailureCount; got != 0 {
		t.Errorf("ActionFailureCount = %d for a non-Lifetime failure, want 0 (never counted)", got)
	}
}
