// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

func readyStageSet() *stagesv1.StageSet {
	return &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "web"},
		Spec: stagesv1.StageSetSpec{
			Stages: []stagesv1.Stage{{Name: "canary"}, {Name: "prod"}},
		},
		Status: stagesv1.StageSetStatus{
			Version: "1.4.2",
			Conditions: []metav1.Condition{{
				Type: conditionReady, Status: metav1.ConditionTrue, Reason: "Succeeded", Message: "all stages applied",
			}},
			Stages: []stagesv1.StageStatus{
				{Name: "canary", Phase: stagesv1.StageReady, AppliedRevision: "sha256:aaa", EntriesCount: 3},
				{Name: "prod", Phase: stagesv1.StageReady, AppliedRevision: "sha256:bbb", EntriesCount: 5},
			},
		},
	}
}

func TestWriteDetail_RendersReadyAndStages(t *testing.T) {
	var buf bytes.Buffer
	writeDetail(&buf, readyStageSet())
	out := buf.String()

	for _, want := range []string{
		"Name:       web",
		"Namespace:  team-a",
		"Ready:      True (Succeeded)",
		"Message:    all stages applied",
		"Version:    1.4.2",
		"canary",
		"prod",
		"Ready",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("detail output missing %q:\n%s", want, out)
		}
	}
}

func TestWriteDetail_PendingUpdate(t *testing.T) {
	ss := readyStageSet()
	open := metav1.NewTime(time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC))
	ss.Status.PendingUpdate = &stagesv1.PendingUpdate{
		Revisions:       map[string]string{"team-a/web-artifact": "sha256:ccc"},
		NextWindowOpens: &open,
	}
	var buf bytes.Buffer
	writeDetail(&buf, ss)
	out := buf.String()

	if !strings.Contains(out, "Pending update:") {
		t.Errorf("missing pending-update block:\n%s", out)
	}
	if !strings.Contains(out, "team-a/web-artifact -> sha256:ccc") {
		t.Errorf("missing held revision:\n%s", out)
	}
	if !strings.Contains(out, "Next window opens") {
		t.Errorf("missing next-window line:\n%s", out)
	}
}

func TestWriteTable_Columns(t *testing.T) {
	var buf bytes.Buffer
	writeTable(&buf, []stagesv1.StageSet{*readyStageSet()}, false)
	out := buf.String()

	if !strings.Contains(out, "NAME") || !strings.Contains(out, "READY") || !strings.Contains(out, "STAGES") {
		t.Errorf("table header incomplete:\n%s", out)
	}
	if !strings.Contains(out, "web") || !strings.Contains(out, "True") || !strings.Contains(out, "2/2") {
		t.Errorf("table row incomplete:\n%s", out)
	}
}

func TestWriteTable_AllNamespacesAddsColumn(t *testing.T) {
	var buf bytes.Buffer
	writeTable(&buf, []stagesv1.StageSet{*readyStageSet()}, true)
	out := buf.String()
	if !strings.Contains(out, "NAMESPACE") || !strings.Contains(out, "team-a") {
		t.Errorf("all-namespaces table missing namespace column:\n%s", out)
	}
}

func TestWriteTable_Empty(t *testing.T) {
	var buf bytes.Buffer
	writeTable(&buf, nil, false)
	if !strings.Contains(buf.String(), "No StageSets found.") {
		t.Errorf("empty table = %q", buf.String())
	}
}

func TestStageProgress_PartiallyReady(t *testing.T) {
	ss := readyStageSet()
	ss.Status.Stages[1].Phase = stagesv1.StageApplying
	if got := stageProgress(ss); got != "1/2" {
		t.Errorf("stageProgress = %q, want 1/2", got)
	}
}

func TestReadyState_MissingConditionIsUnknown(t *testing.T) {
	ss := &stagesv1.StageSet{}
	ready, reason := readyState(ss)
	if ready != "Unknown" || reason != "-" {
		t.Errorf("readyState(no conditions) = %q,%q want Unknown,-", ready, reason)
	}
}

// --- envtest integration ---

func TestGet_ListAndDetail(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "get")
	makeStageSet(t, c, ns, "alpha")
	makeStageSet(t, c, ns, "beta")

	stdout, stderr, code := runCLI(t, cfg, "get", "-n", ns)
	if code != exitOK {
		t.Fatalf("get list exit = %d (stderr=%s)", code, stderr)
	}
	if !strings.Contains(stdout, "alpha") || !strings.Contains(stdout, "beta") {
		t.Errorf("list missing StageSets:\n%s", stdout)
	}

	stdout, stderr, code = runCLI(t, cfg, "get", "alpha", "-n", ns)
	if code != exitOK {
		t.Fatalf("get detail exit = %d (stderr=%s)", code, stderr)
	}
	if !strings.Contains(stdout, "Name:       alpha") {
		t.Errorf("detail missing name:\n%s", stdout)
	}
}

func TestGet_YAMLOutput(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "getyaml")
	makeStageSet(t, c, ns, "gamma")

	stdout, stderr, code := runCLI(t, cfg, "get", "gamma", "-n", ns, "-o", "yaml")
	if code != exitOK {
		t.Fatalf("get -o yaml exit = %d (stderr=%s)", code, stderr)
	}
	if !strings.Contains(stdout, "kind: StageSet") || !strings.Contains(stdout, "name: gamma") {
		t.Errorf("yaml output unexpected:\n%s", stdout)
	}
}

func TestGet_NotFound_ExitsError(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "getmiss")

	_, stderr, code := runCLI(t, cfg, "get", "ghost", "-n", ns)
	if code != exitError {
		t.Fatalf("get missing exit = %d, want %d", code, exitError)
	}
	if !strings.Contains(stderr, "not found") {
		t.Errorf("stderr missing 'not found':\n%s", stderr)
	}
}

// `get NAME -A` is a contradiction (a name is namespaced) — it must error
// loudly, not silently ignore --all-namespaces and look only in the current ns.
func TestGet_NameWithAllNamespacesErrors(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "getnameall")
	makeStageSet(t, c, ns, "alpha")

	_, stderr, code := runCLI(t, cfg, "get", "alpha", "-n", ns, "--all-namespaces")
	// A name + --all-namespaces is flag misuse, so it exits with the usage code
	// (2), not the runtime-failure code (3).
	if code != exitUsage {
		t.Fatalf("get NAME --all-namespaces exit = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, "across all namespaces") {
		t.Errorf("stderr should explain the name/-A conflict:\n%s", stderr)
	}
}
