// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

func makeGatedStageSet(t testing.TB, c client.Client, ns, name string) {
	t.Helper()
	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: stagesv1.StageSetSpec{
			Interval: metav1.Duration{Duration: 5 * time.Minute},
			Stages: []stagesv1.Stage{{
				Name:      "first",
				SourceRef: stagesv1.SourceReference{Name: name + "-artifact"},
				Promotion: &stagesv1.StagePromotion{RequireManualPromotion: true},
			}},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
}

// setStageStatus records st as the StageSet's only stage status, as the
// controller would after a reconcile.
func setStageStatus(t testing.TB, c client.Client, ns, name string, st stagesv1.StageStatus) {
	t.Helper()
	var ss stagesv1.StageSet
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &ss); err != nil {
		t.Fatalf("get StageSet: %v", err)
	}
	ss.Status.Stages = []stagesv1.StageStatus{st}
	if err := c.Status().Update(context.Background(), &ss); err != nil {
		t.Fatalf("update StageSet status: %v", err)
	}
}

func heldStatus(phase stagesv1.PromotionPhase, rev string) stagesv1.StageStatus {
	return stagesv1.StageStatus{
		Name:            "first",
		Phase:           stagesv1.StageReady,
		AppliedRevision: rev,
		PromotionState:  &stagesv1.PromotionState{Phase: phase},
	}
}

func TestPromote_StampsAnnotation(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "promote")
	makeGatedStageSet(t, c, ns, "app")
	setStageStatus(t, c, ns, "app", heldStatus(stagesv1.PromotionAwaitingManual, "rev1@sha256:aaa"))

	stdout, stderr, code := runCLI(t, cfg, "promote", "app", "-n", ns, "--stage", "first")
	if code != exitOK {
		t.Fatalf("promote exit = %d (stderr=%s)", code, stderr)
	}
	if !strings.Contains(stdout, "Promotion requested") {
		t.Errorf("missing confirmation:\n%s", stdout)
	}
	if val := getAnnotations(t, c, ns, "app")[promoteAnnotation]; !strings.HasPrefix(val, "first@") {
		t.Errorf("promote annotation = %q, want first@<token>", val)
	}
	if val := getAnnotations(t, c, ns, "app")[promoteRevisionAnnotation]; val != "rev1@sha256:aaa" {
		t.Errorf("promote-revision annotation = %q, want the held revision", val)
	}
}

func TestPromote_BindsTheAbortedRevision(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "promoteabort")
	makeGatedStageSet(t, c, ns, "app")
	st := heldStatus(stagesv1.PromotionBlocked, "rev1@sha256:aaa")
	st.PromotionState.AbortedRevision = "rev2@sha256:bbb"
	setStageStatus(t, c, ns, "app", st)

	if _, stderr, code := runCLI(t, cfg, "promote", "app", "-n", ns, "--stage", "first"); code != exitOK {
		t.Fatalf("promote exit = %d (stderr=%s)", code, stderr)
	}
	if val := getAnnotations(t, c, ns, "app")[promoteRevisionAnnotation]; val != "rev2@sha256:bbb" {
		t.Errorf("promote-revision annotation = %q, want the aborted revision", val)
	}
}

func TestPromote_RefusesStageNotHeld(t *testing.T) {
	cases := map[string]*stagesv1.StageStatus{
		"not reached": nil,
		"promoted":    func() *stagesv1.StageStatus { s := heldStatus(stagesv1.PromotionPromoted, "rev1"); return &s }(),
		"failed":      {Name: "first", Phase: stagesv1.StageFailed, AppliedRevision: "rev1"},
	}
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	i := 0
	for name, st := range cases {
		t.Run(name, func(t *testing.T) {
			i++
			ns := makeNamespace(t, c, fmt.Sprintf("promotenotheld%d", i))
			makeGatedStageSet(t, c, ns, "app")
			if st != nil {
				setStageStatus(t, c, ns, "app", *st)
			}
			_, stderr, code := runCLI(t, cfg, "promote", "app", "-n", ns, "--stage", "first")
			if code == exitOK {
				t.Fatal("promoting a stage not held at its gate should fail")
			}
			if !strings.Contains(stderr, "would ignore the promotion") {
				t.Errorf("stderr should explain the refusal:\n%s", stderr)
			}
			if _, ok := getAnnotations(t, c, ns, "app")[promoteAnnotation]; ok {
				t.Error("a refused promotion must not stamp the annotation")
			}
		})
	}
}

func TestWaitForPromote(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	opts := promoteOptions{name: "app", stage: "first", timeout: 10 * time.Second}

	t.Run("handled", func(t *testing.T) {
		ns := makeNamespace(t, c, "promotewaitok")
		makeGatedStageSet(t, c, ns, "app")
		st := heldStatus(stagesv1.PromotionPromoted, "rev1")
		st.LastHandledPromotion = "tok"
		setStageStatus(t, c, ns, "app", st)
		ss := &stagesv1.StageSet{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "app"}}
		if err := waitForPromote(context.Background(), c, ss, opts, "tok"); err != nil {
			t.Fatalf("waitForPromote: %v", err)
		}
	})
	t.Run("ignored", func(t *testing.T) {
		ns := makeNamespace(t, c, "promotewaitign")
		makeGatedStageSet(t, c, ns, "app")
		st := heldStatus(stagesv1.PromotionSoaking, "rev2")
		st.IgnoredPromotion = &stagesv1.IgnoredPromotion{Token: "tok", Reason: stagesv1.PromoteIgnoredRevisionMismatch, Message: "issued for rev1"}
		setStageStatus(t, c, ns, "app", st)
		ss := &stagesv1.StageSet{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "app"}}
		err := waitForPromote(context.Background(), c, ss, opts, "tok")
		if !errors.Is(err, errPromotionIgnored) || !strings.Contains(err.Error(), "issued for rev1") {
			t.Fatalf("err = %v, want the controller's refusal", err)
		}
	})
	t.Run("another ignored token keeps waiting", func(t *testing.T) {
		ns := makeNamespace(t, c, "promotewaitother")
		makeGatedStageSet(t, c, ns, "app")
		st := heldStatus(stagesv1.PromotionSoaking, "rev2")
		st.IgnoredPromotion = &stagesv1.IgnoredPromotion{Token: "older", Reason: stagesv1.PromoteIgnoredNotHeld}
		setStageStatus(t, c, ns, "app", st)
		ss := &stagesv1.StageSet{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "app"}}
		short := opts
		short.timeout = 2 * time.Second
		err := waitForPromote(context.Background(), c, ss, short, "tok")
		if err == nil || errors.Is(err, errPromotionIgnored) {
			t.Fatalf("err = %v, want a timeout", err)
		}
	})
}

func TestPromote_RequiresStageFlag(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "promotenostg")
	makeGatedStageSet(t, c, ns, "app")

	if _, _, code := runCLI(t, cfg, "promote", "app", "-n", ns); code == exitOK {
		t.Error("promote without --stage should fail")
	}
}

func TestPromote_UnknownStage(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "promotebadstg")
	makeGatedStageSet(t, c, ns, "app")

	if _, _, code := runCLI(t, cfg, "promote", "app", "-n", ns, "--stage", "nope"); code == exitOK {
		t.Error("promote of an unknown stage should fail")
	}
}

func TestPromote_StageWithoutGate(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "promotenogate")
	makeStageSet(t, c, ns, "app") // stage "first", no promotion gate

	if _, _, code := runCLI(t, cfg, "promote", "app", "-n", ns, "--stage", "first"); code == exitOK {
		t.Error("promoting a stage with no promotion gate should fail")
	}
}
