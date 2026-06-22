// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/client-go/rest"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/preview"
)

// --- exitErr / runtimeErr / usageErr ---

func TestExitErr_ErrorWithWrappedErr(t *testing.T) {
	e := &exitErr{code: exitError, err: errors.New("boom")}
	if got := e.Error(); got != "boom" {
		t.Errorf("Error() = %q, want %q", got, "boom")
	}
	if !errors.Is(e, e.err) {
		t.Error("Unwrap should expose the wrapped error")
	}
}

func TestExitErr_ErrorWithoutWrappedErr(t *testing.T) {
	e := &exitErr{code: exitDiff}
	if got := e.Error(); got != "exit 1" {
		t.Errorf("Error() = %q, want %q", got, "exit 1")
	}
	if e.Unwrap() != nil {
		t.Error("Unwrap of a code-only exitErr should be nil")
	}
}

func TestRuntimeErr_Classification(t *testing.T) {
	if runtimeErr(nil) != nil {
		t.Error("runtimeErr(nil) must be nil")
	}
	var ee *exitErr
	if !errors.As(runtimeErr(errors.New("x")), &ee) || ee.code != exitError {
		t.Errorf("runtimeErr should wrap as exitError (%d), got %+v", exitError, ee)
	}
}

func TestUsageErr_Classification(t *testing.T) {
	if usageErr(nil) != nil {
		t.Error("usageErr(nil) must be nil")
	}
	var ee *exitErr
	if !errors.As(usageErr(errors.New("x")), &ee) || ee.code != exitUsage {
		t.Errorf("usageErr should wrap as exitUsage (%d), got %+v", exitUsage, ee)
	}
}

// --- *-or-dash helpers: empty-string branch ---

func TestOrDashHelpers_EmptyAndNonEmpty(t *testing.T) {
	if versionOrDash("") != "-" || versionOrDash("1.2.3") != "1.2.3" {
		t.Error("versionOrDash branches incorrect")
	}
	if revisionOrDash("") != "-" || revisionOrDash("sha256:x") != "sha256:x" {
		t.Error("revisionOrDash branches incorrect")
	}
	if phaseOrDash("") != "-" {
		t.Error("phaseOrDash empty should be dash")
	}
	if phaseOrDash(stagesv1.StageReady) != string(stagesv1.StageReady) {
		t.Error("phaseOrDash non-empty should echo the phase")
	}
}

// --- pendingSummary: all three branches ---

func TestPendingSummary_Branches(t *testing.T) {
	t.Run("no pending update", func(t *testing.T) {
		if got := pendingSummary(&stagesv1.StageSet{}); got != "-" {
			t.Errorf("no pending update = %q, want -", got)
		}
	})

	t.Run("held without window", func(t *testing.T) {
		ss := &stagesv1.StageSet{}
		ss.Status.PendingUpdate = &stagesv1.PendingUpdate{
			Revisions: map[string]string{"ns/art": "sha256:x"},
		}
		if got := pendingSummary(ss); got != "held" {
			t.Errorf("held no window = %q, want held", got)
		}
	})

	t.Run("held until window opens", func(t *testing.T) {
		open := metav1.NewTime(time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC))
		ss := &stagesv1.StageSet{}
		ss.Status.PendingUpdate = &stagesv1.PendingUpdate{NextWindowOpens: &open}
		got := pendingSummary(ss)
		if !strings.HasPrefix(got, "held until ") {
			t.Errorf("held with window = %q, want 'held until ...'", got)
		}
	})
}

// --- sortedKeys ---

func TestSortedKeys(t *testing.T) {
	if got := sortedKeys(nil); len(got) != 0 {
		t.Errorf("sortedKeys(nil) = %v, want empty", got)
	}
	got := sortedKeys(map[string]string{"c": "", "a": "", "b": ""})
	if strings.Join(got, ",") != "a,b,c" {
		t.Errorf("sortedKeys = %v, want a,b,c", got)
	}
}

// --- indexStageStatuses ---

func TestIndexStageStatuses(t *testing.T) {
	in := []stagesv1.StageStatus{
		{Name: "a", EntriesCount: 1},
		{Name: "b", EntriesCount: 2},
	}
	out := indexStageStatuses(in)
	if len(out) != 2 {
		t.Fatalf("index size = %d, want 2", len(out))
	}
	if out["a"].EntriesCount != 1 || out["b"].EntriesCount != 2 {
		t.Errorf("index content wrong: %+v", out)
	}
	if empty := indexStageStatuses(nil); len(empty) != 0 {
		t.Errorf("nil index = %v, want empty", empty)
	}
}

// --- encodeObject: json, yaml, and the marshal-error path ---

func TestEncodeObject_JSON(t *testing.T) {
	var buf bytes.Buffer
	if err := encodeObject(&buf, map[string]any{"k": "v"}, "json"); err != nil {
		t.Fatalf("json encode: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `"k": "v"`) {
		t.Errorf("json output = %q, want indented k/v", out)
	}
}

func TestEncodeObject_YAML(t *testing.T) {
	var buf bytes.Buffer
	if err := encodeObject(&buf, map[string]any{"k": "v"}, "yaml"); err != nil {
		t.Fatalf("yaml encode: %v", err)
	}
	if !strings.Contains(buf.String(), "k: v") {
		t.Errorf("yaml output = %q, want k: v", buf.String())
	}
}

func TestEncodeObject_YAMLMarshalError(t *testing.T) {
	// A channel cannot be marshaled to YAML, exercising the error return.
	if err := encodeObject(&bytes.Buffer{}, make(chan int), "yaml"); err == nil {
		t.Error("expected a marshal error for an unmarshalable value")
	}
}

// --- writeDetail: branches not hit by get_test's ready fixture ---

func TestWriteDetail_SuspendedAndNoCondition(t *testing.T) {
	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "svc"},
		Spec:       stagesv1.StageSetSpec{Suspend: true},
	}
	var buf bytes.Buffer
	writeDetail(&buf, ss)
	out := buf.String()
	if !strings.Contains(out, "Suspended:  true") {
		t.Errorf("missing suspended line:\n%s", out)
	}
	// With no Ready condition the detail falls back to Unknown.
	if !strings.Contains(out, "Ready:      Unknown") {
		t.Errorf("missing Unknown readiness fallback:\n%s", out)
	}
}

func TestWriteDetail_PendingMigrationsAndLastHandled(t *testing.T) {
	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "svc"},
	}
	ss.Status.PendingMigrations = []stagesv1.PendingMigration{
		{Name: "m1"}, {Name: "m2"},
	}
	ss.Status.SetLastHandledReconcileRequest("token-xyz")
	var buf bytes.Buffer
	writeDetail(&buf, ss)
	out := buf.String()
	if !strings.Contains(out, "Pending migrations: m1, m2") {
		t.Errorf("missing pending migrations line:\n%s", out)
	}
	if !strings.Contains(out, "Last handled reconcile: token-xyz") {
		t.Errorf("missing last-handled-reconcile line:\n%s", out)
	}
}

// --- reconcileHandled: branches the wait_test fixtures don't drive ---

func TestReconcileHandled_StageLevel(t *testing.T) {
	opts := reconcileOptions{stage: "prod"}
	ss := &stagesv1.StageSet{}

	// Stage absent from status: not handled.
	if reconcileHandled(ss, opts, "tok") {
		t.Error("stage missing from status should not be handled")
	}

	// Stage present but token mismatches: not handled.
	ss.Status.Stages = []stagesv1.StageStatus{{Name: "prod", LastHandledReconcileAt: "other"}}
	if reconcileHandled(ss, opts, "tok") {
		t.Error("stale stage token should not be handled")
	}

	// Token matches: handled.
	ss.Status.Stages[0].LastHandledReconcileAt = "tok"
	if !reconcileHandled(ss, opts, "tok") {
		t.Error("matching stage token should be handled")
	}

	// --update-now with a still-pending update at stage level: not handled.
	optsUpdateNow := reconcileOptions{stage: "prod", updateNow: true}
	ss.Status.PendingUpdate = &stagesv1.PendingUpdate{Revisions: map[string]string{"a": "b"}}
	if reconcileHandled(ss, optsUpdateNow, "tok") {
		t.Error("stage update-now with pending update should not be handled")
	}
}

func TestReconcileHandled_StageSetLevelUpdateNow(t *testing.T) {
	ss := &stagesv1.StageSet{}
	ss.Status.SetLastHandledReconcileRequest("tok")

	// Token matched, no pending update: handled.
	if !reconcileHandled(ss, reconcileOptions{}, "tok") {
		t.Error("matched token with no pending update should be handled")
	}

	// --update-now still blocked while an update is pending.
	ss.Status.PendingUpdate = &stagesv1.PendingUpdate{Revisions: map[string]string{"a": "b"}}
	if reconcileHandled(ss, reconcileOptions{updateNow: true}, "tok") {
		t.Error("update-now with pending update should not be handled")
	}

	// Mismatched StageSet-level token: not handled.
	if reconcileHandled(ss, reconcileOptions{}, "different") {
		t.Error("mismatched StageSet token should not be handled")
	}
}

// --- specStage ---

func TestSpecStage(t *testing.T) {
	ss := &stagesv1.StageSet{
		Spec: stagesv1.StageSetSpec{Stages: []stagesv1.Stage{{Name: "a"}, {Name: "b"}}},
	}
	if st := specStage(ss, "b"); st == nil || st.Name != "b" {
		t.Errorf("specStage(b) = %+v, want stage b", st)
	}
	if specStage(ss, "missing") != nil {
		t.Error("specStage(missing) should be nil")
	}
}

// --- readLadderFiles: the directory walk branch (37.5%) ---

func TestReadLadderFiles_Directory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.yaml"), []byte("a: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "b.yaml"), []byte("b: 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	files, err := readLadderFiles(dir)
	if err != nil {
		t.Fatalf("readLadderFiles(dir): %v", err)
	}
	if got := files["a.yaml"]; got != "a: 1\n" {
		t.Errorf("a.yaml content = %q", got)
	}
	// Nested files are keyed by their path relative to the root.
	if got := files[filepath.Join("sub", "b.yaml")]; got != "b: 2\n" {
		t.Errorf("sub/b.yaml content = %q (keys=%v)", got, files)
	}
}

func TestReadLadderFiles_SingleFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "only.yaml")
	if err := os.WriteFile(p, []byte("x: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	files, err := readLadderFiles(p)
	if err != nil {
		t.Fatalf("readLadderFiles(file): %v", err)
	}
	if len(files) != 1 || files["only.yaml"] != "x: 1\n" {
		t.Errorf("single-file read = %v", files)
	}
}

func TestReadLadderFiles_Missing(t *testing.T) {
	if _, err := readLadderFiles(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Error("readLadderFiles of a missing path should error")
	}
}

// --- namespace(): explicit flag wins; default fallback under an injected config ---

func TestNamespace_ExplicitFlagWins(t *testing.T) {
	chosen := "chosen"
	o := &options{configFlags: genericclioptions.NewConfigFlags(true)}
	o.configFlags.Namespace = &chosen
	if got := o.namespace(); got != "chosen" {
		t.Errorf("explicit -n = %q, want chosen", got)
	}
}

func TestNamespace_DefaultUnderInjectedConfig(t *testing.T) {
	// With an injected restConfigOverride and no -n flag, the kubeconfig context
	// is never consulted, so the fallback is "default".
	empty := ""
	o := &options{
		configFlags:        genericclioptions.NewConfigFlags(true),
		restConfigOverride: &rest.Config{},
	}
	o.configFlags.Namespace = &empty
	if got := o.namespace(); got != "default" {
		t.Errorf("default fallback = %q, want default", got)
	}
}

// --- Run: the public entry point, exercised via the cluster-free --help path ---

func TestRun_PublicEntryPoint_Help(t *testing.T) {
	var out, errb bytes.Buffer
	streams := genericiooptions.IOStreams{In: strings.NewReader(""), Out: &out, ErrOut: &errb}
	code := Run(t.Context(), streams, []string{"--help"})
	if code != exitOK {
		t.Fatalf("Run --help exit = %d, want %d (stderr=%s)", code, exitOK, errb.String())
	}
	if !strings.Contains(out.String(), "stagesetctl") {
		t.Errorf("Run --help output missing command name:\n%s", out.String())
	}
}

// --- reportTransition: the bad-semver --from / --to error branches ---

func TestLintMigrations_BadFromSemver(t *testing.T) {
	path := writeLadder(t, "- name: a\n  to: \"2.0.0\"\n  stage: s\n")
	_, stderr, code := runCLI(t, nil, "lint-migrations", path, "--from", "not-a-version", "--to", "2.0.0")
	if code != exitError {
		t.Fatalf("bad --from exit = %d, want %d", code, exitError)
	}
	if !strings.Contains(stderr, "--from") {
		t.Errorf("stderr should name the bad --from value:\n%s", stderr)
	}
}

func TestLintMigrations_BadToSemver(t *testing.T) {
	path := writeLadder(t, "- name: a\n  to: \"2.0.0\"\n  stage: s\n")
	_, stderr, code := runCLI(t, nil, "lint-migrations", path, "--from", "1.0.0", "--to", "not-a-version")
	if code != exitError {
		t.Fatalf("bad --to exit = %d, want %d", code, exitError)
	}
	if !strings.Contains(stderr, "--to") {
		t.Errorf("stderr should name the bad --to value:\n%s", stderr)
	}
}

// --- command Get-error paths: a missing StageSet routes through runtimeErr ---

func TestBuild_MissingStageSet_ExitsError(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "buildmiss")
	_, stderr, code := runCLI(t, cfg, "build", "ghost", "-n", ns)
	if code != exitError {
		t.Fatalf("build missing StageSet exit = %d, want %d (stderr=%s)", code, exitError, stderr)
	}
	if !strings.Contains(stderr, "not found") {
		t.Errorf("stderr missing 'not found':\n%s", stderr)
	}
}

func TestDiff_MissingStageSet_ExitsError(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "diffmiss")
	_, stderr, code := runCLI(t, cfg, "diff", "ghost", "-n", ns)
	if code != exitError {
		t.Fatalf("diff missing StageSet exit = %d, want %d (stderr=%s)", code, exitError, stderr)
	}
}

func TestApply_MissingStageSet_ExitsError(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "applymiss")
	_, stderr, code := runCLI(t, cfg, "apply", "ghost", "-n", ns)
	if code != exitError {
		t.Fatalf("apply missing StageSet exit = %d, want %d (stderr=%s)", code, exitError, stderr)
	}
}

func TestReconcile_MissingStageSet_ExitsError(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "reconcilemiss")
	_, stderr, code := runCLI(t, cfg, "reconcile", "ghost", "-n", ns)
	if code != exitError {
		t.Fatalf("reconcile missing StageSet exit = %d, want %d (stderr=%s)", code, exitError, stderr)
	}
}

// --- a --source-dir parse error fails before any client is built (cluster-free) ---

func TestBuild_BadSourceDir_ExitsError(t *testing.T) {
	_, stderr, code := runArgs("build", "anything", "--source-dir", "=")
	if code != exitError {
		t.Fatalf("bad --source-dir exit = %d, want %d (stderr=%s)", code, exitError, stderr)
	}
	if !strings.Contains(stderr, "empty path") {
		t.Errorf("stderr missing 'empty path':\n%s", stderr)
	}
}

// --- reconcileSources: a missing source is a warning that increments the count ---

func TestReconcileSources_MissingSourceWarns(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "rsmiss")
	// makeStageSet's single stage references "<name>-artifact", which is never
	// created here, so the source Get fails and the count of failures is 1.
	ss := makeStageSet(t, c, ns, "app")

	var errb bytes.Buffer
	failed, err := reconcileSources(t.Context(), c, ss, newToken(), &errb)
	if err != nil {
		t.Fatalf("reconcileSources returned an aborting error: %v", err)
	}
	if failed != 1 {
		t.Errorf("failed count = %d, want 1", failed)
	}
	if !strings.Contains(errb.String(), "warning: cannot reconcile source") {
		t.Errorf("missing per-source warning:\n%s", errb.String())
	}
}

// --- newClient / impersonatedClient: the mapper-construction error branch ---

// brokenConfig is a rest.Config whose TLS settings cannot produce an HTTP
// client, so building a discovery-backed RESTMapper (apiMapperFor) fails — which
// exercises newClient's and impersonatedClient's mapper-error returns without a
// live apiserver.
func brokenConfig() *rest.Config {
	return &rest.Config{
		Host: "https://127.0.0.1:1",
		TLSClientConfig: rest.TLSClientConfig{
			CAData:   []byte("not-a-pem"),
			CAFile:   "/nonexistent/ca.pem", // CAData + CAFile together is rejected
			Insecure: false,
		},
	}
}

func TestNewClient_MapperError(t *testing.T) {
	o := &options{
		configFlags:        genericclioptions.NewConfigFlags(true),
		restConfigOverride: brokenConfig(),
	}
	if _, _, err := o.newClient(); err == nil {
		t.Fatal("newClient with a broken config should error")
	}
}

func TestImpersonatedClient_MapperError(t *testing.T) {
	o := &options{
		configFlags:        genericclioptions.NewConfigFlags(true),
		restConfigOverride: brokenConfig(),
	}
	if _, err := o.impersonatedClient("ns", "sa"); err == nil {
		t.Fatal("impersonatedClient with a broken config should error")
	}
}

func TestApiMapperFor_BrokenConfigError(t *testing.T) {
	if _, err := apiMapperFor(brokenConfig()); err == nil {
		t.Fatal("apiMapperFor with a broken config should error")
	}
}

// --- render-error branch: a --source-dir pointing at a nonexistent tree makes
//     RenderStage fail, exercising the per-stage error return in build and diff ---

func TestBuild_RenderError_FromMissingSourceDir(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "buildrender")
	makeStageSet(t, c, ns, "app")
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	_, stderr, code := runCLI(t, cfg, "build", "app", "-n", ns, "--source-dir", missing)
	if code != exitError {
		t.Fatalf("build with missing source-dir exit = %d, want %d (stderr=%s)", code, exitError, stderr)
	}
}

func TestDiff_RenderError_FromMissingSourceDir(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "diffrender")
	makeStageSet(t, c, ns, "app")
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	_, stderr, code := runCLI(t, cfg, "diff", "app", "-n", ns, "--source-dir", missing)
	// A render failure is a runtime error (exit 3), distinct from clean-diff (1).
	if code != exitError {
		t.Fatalf("diff with missing source-dir exit = %d, want %d (stderr=%s)", code, exitError, stderr)
	}
}

// --- SelectStages sanity through the package boundary the build/diff paths rely
//     on — guards the empty-selection no-op the for-loops in build/diff depend on.

func TestSelectStages_AllWhenUnfiltered(t *testing.T) {
	ss := &stagesv1.StageSet{
		Spec: stagesv1.StageSetSpec{Stages: []stagesv1.Stage{{Name: "a"}, {Name: "b"}}},
	}
	out, err := preview.SelectStages(ss, nil)
	if err != nil {
		t.Fatalf("SelectStages: %v", err)
	}
	if len(out) != 2 {
		t.Errorf("unfiltered selection = %d stages, want 2", len(out))
	}
}
