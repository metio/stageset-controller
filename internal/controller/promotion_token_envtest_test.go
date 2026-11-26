// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"net/http"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/artifact"
)

// stampPromote writes the promote annotations onto the StageSet, as
// stagesetctl promote or a kubectl annotate would.
func stampPromote(t *testing.T, c client.Client, ns, name, value, boundRev string) {
	t.Helper()
	cur := getStageSet(t, c, ns, name)
	if cur.Annotations == nil {
		cur.Annotations = map[string]string{}
	}
	cur.Annotations[promoteAnnotation] = value
	if boundRev == "" {
		delete(cur.Annotations, promoteRevisionAnnotation)
	} else {
		cur.Annotations[promoteRevisionAnnotation] = boundRev
	}
	if err := c.Update(context.Background(), cur); err != nil {
		t.Fatalf("stamp promote annotation: %v", err)
	}
}

// A promote token stamped on a stage that already cleared its gate is refused
// and recorded, and the next revision waits at the manual gate instead of
// promoting on the leftover annotation.
func TestReconcile_Promotion_TokenOnPromotedStageDoesNotCarryOver(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	servedArtifact(t, c, ns, "ea", "", map[string]string{"cm.yaml": cmValManifest(ns, "gated", "v1")})

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "carry"},
		Spec: stagesv1.StageSetSpec{
			Interval: metav1.Duration{Duration: time.Minute},
			Stages: []stagesv1.Stage{{
				Name:      "stage-a",
				SourceRef: stagesv1.SourceReference{Name: "ea"},
				Promotion: &stagesv1.StagePromotion{RequireManualPromotion: true},
			}},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	rec := &capturingRecorder{}
	now := tm("2026-01-01T00:00:00Z").Time
	pass := func(at time.Time) {
		t.Helper()
		r := &StageSetReconciler{
			Client:     c,
			RESTMapper: c.RESTMapper(),
			Recorder:   rec,
			Fetcher:    &artifact.Fetcher{HTTPClient: http.DefaultClient, URLValidator: artifact.PermissiveHTTPURL, IPValidator: artifact.PermissiveIP},
			Now:        func() time.Time { return at },
		}
		_, _ = driveReconcile(r, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "carry"}})
	}

	// Revision 1 holds at the manual gate, and a token for the held stage
	// promotes it.
	pass(now)
	if got := readyReason(getStageSet(t, c, ns, "carry")); got != ReasonAwaitingPromotion {
		t.Fatalf("Ready reason = %q, want %q", got, ReasonAwaitingPromotion)
	}
	stampPromote(t, c, ns, "carry", "stage-a@tok1", "")
	pass(now.Add(time.Minute))
	if got := readyReason(getStageSet(t, c, ns, "carry")); got != ReasonReady {
		t.Fatalf("after promote, Ready reason = %q, want %q", got, ReasonReady)
	}

	// A second token arrives while the stage is already promoted.
	stampPromote(t, c, ns, "carry", "stage-a@tok2", "")
	pass(now.Add(2 * time.Minute))
	got := getStageSet(t, c, ns, "carry")
	sa := stageStatusFor(got, "stage-a")
	if sa == nil || sa.LastHandledPromotion != "tok1" {
		t.Fatalf("lastHandledPromotion = %+v, want tok1", sa)
	}
	if sa.IgnoredPromotion == nil || sa.IgnoredPromotion.Token != "tok2" || sa.IgnoredPromotion.Reason != stagesv1.PromoteIgnoredAlreadyPromoted {
		t.Fatalf("ignoredPromotion = %+v, want tok2 refused as AlreadyPromoted", sa.IgnoredPromotion)
	}
	if n := rec.countEvents(eventReasonPromotionIgnored, `"tok2"`); n != 1 {
		t.Fatalf("PromotionIgnored events for tok2 = %d, want 1", n)
	}

	// Revision 2 arrives with tok2 still on the object: the stage applies it and
	// waits for an operator.
	rev2 := repointArtifact(t, c, ns, "ea", map[string]string{"cm.yaml": cmValManifest(ns, "gated", "v2")})
	pass(now.Add(3 * time.Minute))
	got = getStageSet(t, c, ns, "carry")
	if r := readyReason(got); r != ReasonAwaitingPromotion {
		t.Fatalf("revision 2 Ready reason = %q, want %q: the leftover token promoted it", r, ReasonAwaitingPromotion)
	}
	sa = stageStatusFor(got, "stage-a")
	if sa == nil || sa.AppliedRevision != rev2 || sa.PromotionState == nil || sa.PromotionState.Phase != stagesv1.PromotionAwaitingManual {
		t.Fatalf("stage-a = %+v, want AwaitingManual at %s", sa, rev2)
	}
	if n := rec.countEvents(eventReasonPromotionIgnored, `"tok2"`); n != 1 {
		t.Fatalf("a refused token is reported once, got %d events", n)
	}

	// A token bound to revision 1 does not promote revision 2.
	stampPromote(t, c, ns, "carry", "stage-a@tok3", "rev1-bogus")
	pass(now.Add(4 * time.Minute))
	got = getStageSet(t, c, ns, "carry")
	if r := readyReason(got); r != ReasonAwaitingPromotion {
		t.Fatalf("a token bound to another revision promoted the stage: Ready reason = %q", r)
	}
	if sa = stageStatusFor(got, "stage-a"); sa.IgnoredPromotion == nil || sa.IgnoredPromotion.Reason != stagesv1.PromoteIgnoredRevisionMismatch {
		t.Fatalf("ignoredPromotion = %+v, want RevisionMismatch", sa.IgnoredPromotion)
	}

	// A token bound to revision 2 promotes it.
	stampPromote(t, c, ns, "carry", "stage-a@tok4", rev2)
	pass(now.Add(5 * time.Minute))
	got = getStageSet(t, c, ns, "carry")
	if r := readyReason(got); r != ReasonReady {
		t.Fatalf("a token bound to the held revision must promote it: Ready reason = %q", r)
	}
	if sa = stageStatusFor(got, "stage-a"); sa.LastHandledPromotion != "tok4" {
		t.Fatalf("lastHandledPromotion = %q, want tok4", sa.LastHandledPromotion)
	}
}
