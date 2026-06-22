// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	fluxmeta "github.com/fluxcd/pkg/apis/meta"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/inventory"
	"github.com/metio/stageset-controller/internal/stageinv"
)

// splitKey splits "<ns>/<name>"; a string without a slash is not splittable.
func TestSplitKey_Variants(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in       string
		wantNs   string
		wantName string
		wantOK   bool
	}{
		{"team-a/web", "team-a", "web", true},
		{"/web", "", "web", true},
		{"team-a/", "team-a", "", true},
		{"no-slash", "", "", false},
		{"", "", "", false},
		{"a/b/c", "a", "b/c", true},
	}
	for _, c := range cases {
		ns, name, ok := splitKey(c.in)
		if ns != c.wantNs || name != c.wantName || ok != c.wantOK {
			t.Errorf("splitKey(%q) = (%q,%q,%v), want (%q,%q,%v)",
				c.in, ns, name, ok, c.wantNs, c.wantName, c.wantOK)
		}
	}
}

// migEntryName strips the "@digest" suffix off a migration ledger key; a bare
// name (no "@") is returned unchanged.
func TestMigEntryName_Variants(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"add-index@sha256:abc": "add-index",
		"drop-col":             "drop-col",
		"@only-digest":         "",
		"a@b@c":                "a",
	}
	for in, want := range cases {
		if got := migEntryName(in); got != want {
			t.Errorf("migEntryName(%q) = %q, want %q", in, got, want)
		}
	}
}

// dependsOnKeys defaults a dependency's empty namespace to the StageSet's own
// and renders each ref as a "<ns>/<name>" key.
func TestDependsOnKeys_DefaultsNamespace(t *testing.T) {
	t.Parallel()
	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "web"},
		Spec: stagesv1.StageSetSpec{
			DependsOn: []fluxmeta.NamespacedObjectReference{
				{Name: "db"},
				{Name: "cache", Namespace: "shared"},
			},
		},
	}
	keys := dependsOnKeys(ss)
	want := map[string]bool{"team-a/db": true, "shared/cache": true}
	if len(keys) != len(want) {
		t.Fatalf("keys = %v, want %d entries", keys, len(want))
	}
	for _, k := range keys {
		if !want[k] {
			t.Errorf("unexpected key %q", k)
		}
	}
}

// stageIndex finds a stage by name and returns -1 for an unknown name.
func TestStageIndex_Lookup(t *testing.T) {
	t.Parallel()
	ss := &stagesv1.StageSet{Spec: stagesv1.StageSetSpec{Stages: []stagesv1.Stage{
		{Name: "canary"}, {Name: "prod"},
	}}}
	if got := stageIndex(ss, "prod"); got != 1 {
		t.Errorf("stageIndex(prod) = %d, want 1", got)
	}
	if got := stageIndex(ss, "canary"); got != 0 {
		t.Errorf("stageIndex(canary) = %d, want 0", got)
	}
	if got := stageIndex(ss, "absent"); got != -1 {
		t.Errorf("stageIndex(absent) = %d, want -1", got)
	}
}

// isReady reports true only when the Ready condition is present and True.
func TestIsReady_ConditionStates(t *testing.T) {
	t.Parallel()
	ready := &stagesv1.StageSet{Status: stagesv1.StageSetStatus{Conditions: []metav1.Condition{
		{Type: ConditionReady, Status: metav1.ConditionTrue},
	}}}
	notReady := &stagesv1.StageSet{Status: stagesv1.StageSetStatus{Conditions: []metav1.Condition{
		{Type: ConditionReady, Status: metav1.ConditionFalse},
	}}}
	absent := &stagesv1.StageSet{}
	if !isReady(ready) {
		t.Error("isReady(Ready=True) = false, want true")
	}
	if isReady(notReady) {
		t.Error("isReady(Ready=False) = true, want false")
	}
	if isReady(absent) {
		t.Error("isReady(no condition) = true, want false")
	}
}

// stagesByPositionDesc orders names by descending recorded position, with the
// name as a stable tie-break for equal positions.
func TestStagesByPositionDesc_Ordering(t *testing.T) {
	t.Parallel()
	records := map[string]stageinv.StageRecord{
		"prod":   {Position: 2},
		"canary": {Position: 1},
		"alpha":  {Position: 1, Refs: []inventory.ObjectRef{{Name: "x"}}},
	}
	got := stagesByPositionDesc(records)
	want := []string{"prod", "alpha", "canary"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// stagePruneByName maps every spec stage to its prune setting, defaulting to
// true when unset and honoring an explicit false.
func TestStagePruneByName_Defaults(t *testing.T) {
	t.Parallel()
	off := false
	on := true
	ss := &stagesv1.StageSet{Spec: stagesv1.StageSetSpec{Stages: []stagesv1.Stage{
		{Name: "default"},
		{Name: "explicit-off", Prune: &off},
		{Name: "explicit-on", Prune: &on},
	}}}
	m := stagePruneByName(ss)
	if !m["default"] {
		t.Error("default stage prune = false, want true")
	}
	if m["explicit-off"] {
		t.Error("explicit-off prune = true, want false")
	}
	if !m["explicit-on"] {
		t.Error("explicit-on prune = false, want true")
	}
}

// stageTimeout prefers the stage timeout, then the StageSet timeout, then the
// 5-minute default; a non-positive value at either level falls through.
func TestStageTimeout_Precedence(t *testing.T) {
	t.Parallel()
	dur := func(d time.Duration) *metav1.Duration { return &metav1.Duration{Duration: d} }

	stageWins := &stagesv1.StageSet{Spec: stagesv1.StageSetSpec{Timeout: dur(time.Minute)}}
	if got := stageTimeout(stageWins, &stagesv1.Stage{Timeout: dur(30 * time.Second)}); got != 30*time.Second {
		t.Errorf("stage timeout = %v, want 30s", got)
	}

	specWins := &stagesv1.StageSet{Spec: stagesv1.StageSetSpec{Timeout: dur(2 * time.Minute)}}
	if got := stageTimeout(specWins, &stagesv1.Stage{Timeout: dur(0)}); got != 2*time.Minute {
		t.Errorf("spec timeout = %v, want 2m (zero stage timeout falls through)", got)
	}

	def := &stagesv1.StageSet{}
	if got := stageTimeout(def, &stagesv1.Stage{}); got != 5*time.Minute {
		t.Errorf("default timeout = %v, want 5m", got)
	}
}

// nextWindowSuffix renders a parenthetical for a real time and nothing for the
// zero time.
func TestNextWindowSuffix(t *testing.T) {
	t.Parallel()
	if got := nextWindowSuffix(time.Time{}); got != "" {
		t.Errorf("zero time suffix = %q, want empty", got)
	}
	at := time.Date(2026, 6, 22, 10, 0, 0, 0, time.UTC)
	got := nextWindowSuffix(at)
	if !strings.Contains(got, "2026-06-22T10:00:00Z") {
		t.Errorf("suffix = %q, want it to carry the RFC3339 time", got)
	}
}

// requeueForWindow clamps to [5s, 1h] around the next window boundary and falls
// back to the supplied interval when there is no boundary.
func TestRequeueForWindow_Clamping(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 22, 10, 0, 0, 0, time.UTC)
	fallback := 7 * time.Minute

	if got := requeueForWindow(time.Time{}, now, fallback); got != fallback {
		t.Errorf("no boundary = %v, want fallback %v", got, fallback)
	}
	if got := requeueForWindow(now.Add(time.Second), now, fallback); got != 5*time.Second {
		t.Errorf("near boundary = %v, want floor 5s", got)
	}
	if got := requeueForWindow(now.Add(3*time.Hour), now, fallback); got != time.Hour {
		t.Errorf("far boundary = %v, want ceil 1h", got)
	}
	if got := requeueForWindow(now.Add(20*time.Minute), now, fallback); got != 20*time.Minute {
		t.Errorf("mid boundary = %v, want 20m exact", got)
	}
}

// tenantRestConfig leaves the minted token as the sole credential and forces
// TLS verification on regardless of the base config's insecure flag.
func TestTenantRestConfig_StripsForeignCredentials(t *testing.T) {
	t.Parallel()
	base := &rest.Config{
		Host:        "https://api.test:6443",
		BearerToken: "controller-token",
		Username:    "admin",
		Password:    "secret",
	}
	base.BearerTokenFile = "/var/run/secrets/token"
	base.Impersonate = rest.ImpersonationConfig{UserName: "someone"}
	base.CAData = []byte("ca-bytes")
	base.ServerName = "api.test"
	base.Insecure = true

	cfg := tenantRestConfig(base, "tenant-token")
	if cfg.BearerToken != "tenant-token" {
		t.Errorf("BearerToken = %q, want the minted tenant token", cfg.BearerToken)
	}
	if cfg.BearerTokenFile != "" {
		t.Error("BearerTokenFile not cleared")
	}
	if cfg.Impersonate.UserName != "" {
		t.Error("Impersonate not cleared")
	}
	if cfg.Username != "" || cfg.Password != "" {
		t.Error("basic-auth credentials not cleared")
	}
	if string(cfg.TLSClientConfig.CAData) != "ca-bytes" || cfg.TLSClientConfig.ServerName != "api.test" {
		t.Error("controller TLS trust not preserved")
	}
	if cfg.TLSClientConfig.Insecure {
		t.Error("Insecure leaked into tenant config; must be forced off")
	}
}

// migrationApprovalPredicate fires only when the approved-version annotation
// actually changes, and never on nil objects.
func TestMigrationApprovalPredicate_Update(t *testing.T) {
	t.Parallel()
	withAnno := func(v string) client.Object {
		o := &stagesv1.StageSet{}
		if v != "" {
			o.SetAnnotations(map[string]string{approvedVersionAnnotation: v})
		}
		return o
	}
	p := migrationApprovalPredicate{}

	if p.Update(event.UpdateEvent{ObjectOld: nil, ObjectNew: withAnno("v2")}) {
		t.Error("nil ObjectOld must not fire")
	}
	if p.Update(event.UpdateEvent{ObjectOld: withAnno("v1"), ObjectNew: nil}) {
		t.Error("nil ObjectNew must not fire")
	}
	if p.Update(event.UpdateEvent{ObjectOld: withAnno("v1"), ObjectNew: withAnno("v1")}) {
		t.Error("unchanged annotation must not fire")
	}
	if !p.Update(event.UpdateEvent{ObjectOld: withAnno(""), ObjectNew: withAnno("v2")}) {
		t.Error("annotation added must fire")
	}
	if !p.Update(event.UpdateEvent{ObjectOld: withAnno("v1"), ObjectNew: withAnno("v2")}) {
		t.Error("annotation changed must fire")
	}
}

// resolvePostBuildVars overlays inline substitute values over substituteFrom
// sources (inline wins), merges ConfigMap and Secret data, and treats a nil
// PostBuild as no variables.
func TestResolvePostBuildVars_MergeAndPrecedence(t *testing.T) {
	t.Parallel()
	const ns = "team-a"
	r := builderWith(
		t,
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "cm"},
			Data:       map[string]string{"region": "eu", "shared": "from-cm"},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "sec"},
			Data:       map[string][]byte{"token": []byte("xyz")},
		},
	)

	if vars, err := r.resolvePostBuildVars(context.Background(), ns, nil); err != nil || vars != nil {
		t.Fatalf("nil PostBuild = (%v,%v), want (nil,nil)", vars, err)
	}

	pb := &stagesv1.PostBuild{
		Substitute: map[string]string{"shared": "inline-wins"},
		SubstituteFrom: []stagesv1.SubstituteReference{
			{Kind: "ConfigMap", Name: "cm"},
			{Kind: "Secret", Name: "sec"},
		},
	}
	vars, err := r.resolvePostBuildVars(context.Background(), ns, pb)
	if err != nil {
		t.Fatalf("resolvePostBuildVars err = %v", err)
	}
	if vars["region"] != "eu" || vars["token"] != "xyz" {
		t.Errorf("merged vars missing source data: %v", vars)
	}
	if vars["shared"] != "inline-wins" {
		t.Errorf("inline substitute did not override substituteFrom: %q", vars["shared"])
	}
}

// A required (non-optional) substituteFrom source that is absent is a hard
// error; the same source marked optional is skipped.
func TestResolvePostBuildVars_MissingSource(t *testing.T) {
	t.Parallel()
	const ns = "team-a"
	r := builderWith(t)

	hard := &stagesv1.PostBuild{SubstituteFrom: []stagesv1.SubstituteReference{
		{Kind: "ConfigMap", Name: "absent"},
	}}
	if _, err := r.resolvePostBuildVars(context.Background(), ns, hard); err == nil {
		t.Fatal("missing required ConfigMap = nil error, want a failure")
	}

	soft := &stagesv1.PostBuild{SubstituteFrom: []stagesv1.SubstituteReference{
		{Kind: "Secret", Name: "absent", Optional: true},
		{Kind: "ConfigMap", Name: "absent", Optional: true},
	}}
	vars, err := r.resolvePostBuildVars(context.Background(), ns, soft)
	if err != nil {
		t.Fatalf("optional missing sources err = %v, want nil", err)
	}
	if len(vars) != 0 {
		t.Errorf("vars = %v, want empty for skipped optional sources", vars)
	}
}

// kubeconfigBytes reads the payload and resourceVersion, defaults the key to
// "value", and errors on a missing Secret or an empty/absent key.
func TestKubeconfigBytes_KeyHandling(t *testing.T) {
	t.Parallel()
	const ns = "team-a"
	r := builderWith(
		t,
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "default-key"},
			Data:       map[string][]byte{"value": []byte("kc-default")},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "custom-key"},
			Data:       map[string][]byte{"config": []byte("kc-custom")},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "empty-key"},
			Data:       map[string][]byte{"value": {}},
		},
	)

	data, ver, err := r.kubeconfigBytes(context.Background(), ns, &fluxmeta.SecretKeyReference{Name: "default-key"})
	if err != nil || string(data) != "kc-default" {
		t.Fatalf("default key = (%q,%v), want kc-default", data, err)
	}
	if ver == "" {
		t.Error("resourceVersion empty; cache invalidation would not track edits")
	}

	data, _, err = r.kubeconfigBytes(context.Background(), ns, &fluxmeta.SecretKeyReference{Name: "custom-key", Key: "config"})
	if err != nil || string(data) != "kc-custom" {
		t.Fatalf("custom key = (%q,%v), want kc-custom", data, err)
	}

	if _, _, err := r.kubeconfigBytes(context.Background(), ns, &fluxmeta.SecretKeyReference{Name: "empty-key"}); err == nil {
		t.Error("empty key value = nil error, want a failure")
	}
	if _, _, err := r.kubeconfigBytes(context.Background(), ns, &fluxmeta.SecretKeyReference{Name: "absent"}); err == nil {
		t.Error("missing Secret = nil error, want a failure")
	}
	if _, _, err := r.kubeconfigBytes(context.Background(), ns, &fluxmeta.SecretKeyReference{Name: "default-key", Key: "nope"}); err == nil {
		t.Error("absent key = nil error, want a failure")
	}
}

// targetCluster with neither a kubeConfig nor a serviceAccountName returns the
// controller's own client and mapper unchanged — the single-tenant default.
func TestTargetCluster_DefaultIdentity(t *testing.T) {
	t.Parallel()
	r := builderWith(t)
	c, m, err := r.targetCluster(context.Background(), "team-a", "", nil)
	if err != nil {
		t.Fatalf("targetCluster err = %v", err)
	}
	if c != r.Client {
		t.Error("default identity did not return the controller client")
	}
	if m != nil && m != r.RESTMapper {
		t.Error("default identity returned an unexpected mapper")
	}
}

// targetCluster with a tenant SA and SkipImpersonation set short-circuits to the
// controller's own client without minting a token (the envtest path).
func TestTargetCluster_SkipImpersonation(t *testing.T) {
	t.Parallel()
	r := builderWith(t)
	r.SkipImpersonation = true
	c, _, err := r.targetCluster(context.Background(), "team-a", "tenant-sa", nil)
	if err != nil {
		t.Fatalf("targetCluster err = %v", err)
	}
	if c != r.Client {
		t.Error("SkipImpersonation path did not return the controller client")
	}
}

// targetCluster asking for a tenant SA without a rest config (so no token can be
// minted) is a configuration error, not a silent fall-through.
func TestTargetCluster_TenantSAWithoutConfig(t *testing.T) {
	t.Parallel()
	r := builderWith(t)
	if _, _, err := r.targetCluster(context.Background(), "team-a", "tenant-sa", nil); err == nil {
		t.Fatal("tenant SA without a rest config = nil error, want a failure")
	}
}
