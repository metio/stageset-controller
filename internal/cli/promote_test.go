// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package cli

import (
	"context"
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

func TestPromote_StampsAnnotation(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "promote")
	makeGatedStageSet(t, c, ns, "app")

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
