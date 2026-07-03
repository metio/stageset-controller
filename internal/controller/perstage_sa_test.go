// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/artifact"
)

// perStageStageSet builds a single-stage StageSet whose spec-level SA is specSA
// and whose one stage overrides it with stageSA (either may be empty).
func perStageStageSet(t *testing.T, c client.Client, ns, name, specSA, stageSA, eaName string) *stagesv1.StageSet {
	t.Helper()
	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: stagesv1.StageSetSpec{
			Interval:           metav1.Duration{Duration: 5 * time.Minute},
			ServiceAccountName: specSA,
			Stages: []stagesv1.Stage{{
				Name:               "stage-a",
				ServiceAccountName: stageSA,
				SourceRef:          stagesv1.SourceReference{Name: eaName},
			}},
		},
	}
	mustCreate(t, c, ss)
	return ss
}

// grantConfigMapsAndSecrets creates a ServiceAccount holding the full verb set on
// both ConfigMaps and Secrets — a strictly broader grant than grantConfigMaps, so
// a stage SA narrowed to ConfigMaps alone is provably tighter than this one.
func grantConfigMapsAndSecrets(t *testing.T, c client.Client, ns, sa string) {
	t.Helper()
	mustCreate(t, c, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: sa}})
	mustCreate(t, c, &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: sa + "-cm-secrets"},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{""},
			Resources: []string{"configmaps", "secrets"},
			Verbs:     []string{"get", "list", "watch", "create", "update", "patch", "delete"},
		}},
	})
	mustCreate(t, c, &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: sa + "-cm-secrets"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: sa + "-cm-secrets"},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Namespace: ns, Name: sa}},
	})
}

// A stage's own serviceAccountName governs its apply even when the StageSet has
// no spec-level serviceAccountName: the stage SA holds the needed RBAC, so the
// apply succeeds under it.
func TestReconcile_PerStageSA_AppliesUnderStageSA(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	grantConfigMaps(t, c, ns, "deployer")
	servedArtifact(t, c, ns, "cm-art", "", map[string]string{"cm.yaml": configMapManifest(ns, "by-stage-sa")})

	ss := perStageStageSet(t, c, ns, "stage-sa-ok", "", "deployer", "cm-art")
	reconcileWithConfig(t, c, ss)

	if r := readyReason(getStageSet(t, c, ns, "stage-sa-ok")); r != ReasonReady {
		t.Fatalf("Ready reason = %q, want %q", r, ReasonReady)
	}
	if !cmExists(t, c, ns, "by-stage-sa") {
		t.Fatal("ConfigMap should have been applied under the stage's ServiceAccount")
	}
}

// A stage's serviceAccountName OVERRIDES the StageSet default and is the ceiling:
// the spec-level SA could create the Secret, but the narrower stage SA cannot, so
// the apply is denied — proving the apply runs as the stage SA, not the broader
// spec SA and not the controller's own identity.
func TestReconcile_PerStageSA_OverridesSpecAndBoundsApply(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	grantConfigMapsAndSecrets(t, c, ns, "broad") // spec-level SA: configmaps + secrets
	grantConfigMaps(t, c, ns, "narrow")          // stage SA: configmaps only
	servedArtifact(t, c, ns, "secret-art", "", map[string]string{"s.yaml": secretManifest(ns, "blocked")})

	ss := perStageStageSet(t, c, ns, "override", "broad", "narrow", "secret-art")
	reconcileWithConfig(t, c, ss)

	if r := readyReason(getStageSet(t, c, ns, "override")); r != ReasonRBACDenied {
		t.Fatalf("Ready reason = %q, want %q (stage SA must bound the apply below the spec SA)", r, ReasonRBACDenied)
	}
	var sec corev1.Secret
	err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "blocked"}, &sec)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("Secret should not exist (stage-SA apply denied), get err = %v", err)
	}
}

// effectiveServiceAccount prefers the stage's own serviceAccountName and falls
// back to the StageSet default when the stage leaves it empty.
func TestEffectiveServiceAccount(t *testing.T) {
	tests := []struct {
		name    string
		specSA  string
		stageSA string
		want    string
	}{
		{"stage overrides spec", "spec-sa", "stage-sa", "stage-sa"},
		{"stage inherits spec", "spec-sa", "", "spec-sa"},
		{"stage set, spec empty", "", "stage-sa", "stage-sa"},
		{"both empty", "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ss := &stagesv1.StageSet{Spec: stagesv1.StageSetSpec{ServiceAccountName: tt.specSA}}
			stage := &stagesv1.Stage{ServiceAccountName: tt.stageSA}
			if got := effectiveServiceAccount(ss, stage); got != tt.want {
				t.Errorf("effectiveServiceAccount = %q, want %q", got, tt.want)
			}
		})
	}
}

// stageRuntime memoizes one runtime per effective SA within a reconcile: the same
// SA returns the identical *stageRuntime (so a shared token/client is reused),
// while a distinct SA gets its own entry. SkipImpersonation keeps both resolves on
// the controller client so the test needs no token minting.
func TestStageRuntime_CachePerSA(t *testing.T) {
	c := testClient(t)
	r := &StageSetReconciler{Client: c, RESTMapper: c.RESTMapper(), SkipImpersonation: true}
	ss := &stagesv1.StageSet{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "x"}}
	fetcher := &artifact.Fetcher{}
	cache := map[string]*stageRuntime{}

	a1, err := r.stageRuntime(context.Background(), ss, "sa-one", fetcher, cache)
	if err != nil {
		t.Fatalf("stageRuntime(sa-one): %v", err)
	}
	a2, err := r.stageRuntime(context.Background(), ss, "sa-one", fetcher, cache)
	if err != nil {
		t.Fatalf("stageRuntime(sa-one) again: %v", err)
	}
	if a1 != a2 {
		t.Error("same effective SA should return the memoized runtime pointer")
	}
	b, err := r.stageRuntime(context.Background(), ss, "sa-two", fetcher, cache)
	if err != nil {
		t.Fatalf("stageRuntime(sa-two): %v", err)
	}
	if a1 == b {
		t.Error("distinct effective SAs should get distinct runtimes")
	}
	if len(cache) != 2 {
		t.Errorf("cache size = %d, want 2", len(cache))
	}
}
