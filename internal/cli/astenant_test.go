// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package cli

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

func optionsFor(cfg *rest.Config) *options {
	return &options{
		configFlags:        genericclioptions.NewConfigFlags(true),
		restConfigOverride: cfg,
	}
}

// TestImpersonatedClient_RBACTakesEffect proves the seam behind --as-tenant: the
// client impersonatedClient returns really acts as the service account, so the
// SA's RBAC — not the caller's — governs the request. A SA granted get on
// ConfigMaps can read one; a SA with no binding is forbidden.
func TestImpersonatedClient_RBACTakesEffect(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "astenant")
	ctx := context.Background()

	probe := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "probe"},
		Data:       map[string]string{"k": "v"},
	}
	if err := c.Create(ctx, probe); err != nil {
		t.Fatalf("create probe ConfigMap: %v", err)
	}

	// "reader" is allowed to get ConfigMaps in ns; "noperms" has no bindings.
	createSA(t, c, ns, "reader")
	createSA(t, c, ns, "noperms")
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "cm-reader"},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"get"},
		}},
	}
	if err := c.Create(ctx, role); err != nil {
		t.Fatalf("create Role: %v", err)
	}
	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "cm-reader"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "cm-reader"},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Namespace: ns, Name: "reader"}},
	}
	if err := c.Create(ctx, rb); err != nil {
		t.Fatalf("create RoleBinding: %v", err)
	}

	o := optionsFor(cfg)

	readerClient, err := o.impersonatedClient(ns, "reader")
	if err != nil {
		t.Fatalf("impersonatedClient(reader): %v", err)
	}
	var got corev1.ConfigMap
	if err := readerClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "probe"}, &got); err != nil {
		t.Fatalf("reader SA should be allowed to get the ConfigMap, got: %v", err)
	}

	noPermsClient, err := o.impersonatedClient(ns, "noperms")
	if err != nil {
		t.Fatalf("impersonatedClient(noperms): %v", err)
	}
	err = noPermsClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "probe"}, &corev1.ConfigMap{})
	if !apierrors.IsForbidden(err) {
		t.Fatalf("noperms SA get should be Forbidden, got: %v", err)
	}
}

// TestBuild_AsTenant_RendersUnderServiceAccount drives the runBuild branch that
// switches to the impersonated client: a StageSet with spec.serviceAccountName,
// built with --as-tenant, renders under that SA (granted enough RBAC here to
// succeed).
func TestBuild_AsTenant_RendersUnderServiceAccount(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "astenantbuild")
	ctx := context.Background()

	createSA(t, c, ns, "renderer")
	crb := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "astenant-renderer-" + ns},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "cluster-admin"},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Namespace: ns, Name: "renderer"}},
	}
	if err := c.Create(ctx, crb); err != nil {
		t.Fatalf("create ClusterRoleBinding: %v", err)
	}
	t.Cleanup(func() { _ = c.Delete(ctx, crb) })

	ss := makeStageSet(t, c, ns, "app")
	ss.Spec.ServiceAccountName = "renderer"
	if err := c.Update(ctx, ss); err != nil {
		t.Fatalf("set serviceAccountName: %v", err)
	}

	dir := writeSourceTree(t, map[string]string{
		"cm.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: settings\ndata:\n  greeting: hello\n",
	})

	stdout, stderr, code := runCLI(t, cfg, "build", "app", "-n", ns, "--source-dir", dir, "--as-tenant")
	if code != exitOK {
		t.Fatalf("build --as-tenant exit = %d (stderr=%s)", code, stderr)
	}
	if !strings.Contains(stdout, "name: settings") {
		t.Errorf("build --as-tenant output unexpected:\n%s", stdout)
	}
}

// TestBuild_AsTenant_UsesPerStageServiceAccount drives the per-stage impersonation
// branch: the StageSet has no spec-level serviceAccountName, but its stage sets
// one, so build --as-tenant renders that stage under the stage's ServiceAccount.
func TestBuild_AsTenant_UsesPerStageServiceAccount(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "astenantstage")
	ctx := context.Background()

	createSA(t, c, ns, "stage-renderer")
	crb := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "astenant-stage-renderer-" + ns},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "cluster-admin"},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Namespace: ns, Name: "stage-renderer"}},
	}
	if err := c.Create(ctx, crb); err != nil {
		t.Fatalf("create ClusterRoleBinding: %v", err)
	}
	t.Cleanup(func() { _ = c.Delete(ctx, crb) })

	ss := makeStageSet(t, c, ns, "app") // no spec-level serviceAccountName
	ss.Spec.Stages[0].ServiceAccountName = "stage-renderer"
	if err := c.Update(ctx, ss); err != nil {
		t.Fatalf("set stage serviceAccountName: %v", err)
	}

	dir := writeSourceTree(t, map[string]string{
		"cm.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: settings\ndata:\n  greeting: hello\n",
	})

	stdout, stderr, code := runCLI(t, cfg, "build", "app", "-n", ns, "--source-dir", dir, "--as-tenant")
	if code != exitOK {
		t.Fatalf("build --as-tenant (per-stage SA) exit = %d (stderr=%s)", code, stderr)
	}
	if !strings.Contains(stdout, "name: settings") {
		t.Errorf("build --as-tenant (per-stage SA) output unexpected:\n%s", stdout)
	}
}

// TestBuild_AsTenant_NoServiceAccountIsNoop covers the branch where --as-tenant
// is set but the StageSet has no serviceAccountName: the command renders with
// the caller's own client and succeeds.
func TestBuild_AsTenant_NoServiceAccountIsNoop(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "astenantnoop")
	makeStageSet(t, c, ns, "app") // no serviceAccountName

	dir := writeSourceTree(t, map[string]string{
		"cm.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: settings\ndata:\n  k: v\n",
	})

	stdout, stderr, code := runCLI(t, cfg, "build", "app", "-n", ns, "--source-dir", dir, "--as-tenant")
	if code != exitOK {
		t.Fatalf("build --as-tenant (no SA) exit = %d (stderr=%s)", code, stderr)
	}
	if !strings.Contains(stdout, "name: settings") {
		t.Errorf("build output unexpected:\n%s", stdout)
	}
}

func createSA(t *testing.T, c client.Client, ns, name string) {
	t.Helper()
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
	if err := c.Create(context.Background(), sa); err != nil {
		t.Fatalf("create ServiceAccount %s: %v", name, err)
	}
}

// TestBuild_AsTenant_ReadsSubstituteFromAsCaller pins the identity split: with
// --as-tenant and a spec.serviceAccountName that has NO RBAC, a build whose
// stage reads postBuild.substituteFrom must still succeed — the controller
// resolves substituteFrom as ITSELF (spec.serviceAccountName scopes only the
// stage's cluster operations), so a faithful preview reads as the caller too.
// Impersonating the read would fail a build the controller performs fine.
func TestBuild_AsTenant_ReadsSubstituteFromAsCaller(t *testing.T) {
	cfg := envtestConfig(t)
	c := testClient(t, cfg)
	ns := makeNamespace(t, c, "astenantsubst")
	ctx := context.Background()

	// The tenant SA exists but is granted NOTHING.
	createSA(t, c, ns, "powerless")

	// A substitution ConfigMap only the caller (envtest admin) can read.
	vars := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "vars"},
		Data:       map[string]string{"greeting": "from-caller"},
	}
	if err := c.Create(ctx, vars); err != nil {
		t.Fatalf("create vars ConfigMap: %v", err)
	}

	ss := makeStageSet(t, c, ns, "app")
	ss.Spec.ServiceAccountName = "powerless"
	ss.Spec.Stages[0].PostBuild = &stagesv1.PostBuild{
		SubstituteFrom: []stagesv1.SubstituteReference{{Kind: "ConfigMap", Name: "vars"}},
	}
	if err := c.Update(ctx, ss); err != nil {
		t.Fatalf("set spec: %v", err)
	}

	dir := writeSourceTree(t, map[string]string{
		"cm.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: settings\ndata:\n  greeting: ${greeting}\n",
	})

	stdout, stderr, code := runCLI(t, cfg, "build", "app", "-n", ns, "--source-dir", dir, "--as-tenant")
	if code != exitOK {
		t.Fatalf("build --as-tenant reading substituteFrom must succeed under a powerless tenant SA (the read runs as the caller); exit=%d stderr=%s", code, stderr)
	}
	if !strings.Contains(stdout, "greeting: from-caller") {
		t.Errorf("substituteFrom value not resolved (read did not run as the caller):\n%s", stdout)
	}
}
