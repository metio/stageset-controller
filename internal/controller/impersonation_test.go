// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"net/http"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/artifact"
)

// reconcileWithConfig runs the reconciler with a rest config wired, so a
// StageSet carrying spec.serviceAccountName actually impersonates and
// spec.kubeConfig actually retargets. The envtest apiserver enforces RBAC, so
// an impersonated SA's bindings bound what the apply can do. The returned error
// is intentionally not asserted: a denied apply both writes the failed status
// (which the tests check) and returns the cause so controller-runtime backs off.
func reconcileWithConfig(t *testing.T, c client.Client, ss *stagesv1.StageSet) {
	t.Helper()
	r := &StageSetReconciler{
		Client:     c,
		Config:     envtestConfig(t),
		RESTMapper: c.RESTMapper(),
		Fetcher:    &artifact.Fetcher{HTTPClient: http.DefaultClient, URLValidator: artifact.PermissiveHTTPURL, IPValidator: artifact.PermissiveIP},
	}
	_, _ = r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: ss.Namespace, Name: ss.Name},
	})
}

// grantConfigMaps creates a ServiceAccount in ns whose only namespace power is
// the full verb set on ConfigMaps — enough for the controller to server-side
// apply, health-check, and prune a ConfigMap under it, but nothing else.
func grantConfigMaps(t *testing.T, c client.Client, ns, sa string) {
	t.Helper()
	mustCreate(t, c, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: sa}})
	mustCreate(t, c, &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: sa + "-configmaps"},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{""},
			Resources: []string{"configmaps"},
			Verbs:     []string{"get", "list", "watch", "create", "update", "patch", "delete"},
		}},
	})
	mustCreate(t, c, &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: sa + "-configmaps"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: sa + "-configmaps"},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Namespace: ns, Name: sa}},
	})
}

func mustCreate(t *testing.T, c client.Client, obj client.Object) {
	t.Helper()
	if err := c.Create(context.Background(), obj); err != nil {
		t.Fatalf("create %T %s: %v", obj, obj.GetName(), err)
	}
}

func secretManifest(ns, name string) string {
	return "apiVersion: v1\nkind: Secret\nmetadata:\n  name: " + name + "\n  namespace: " + ns + "\nstringData:\n  key: value\n"
}

func impersonatingStageSet(t *testing.T, c client.Client, ns, name, sa, eaName string) *stagesv1.StageSet {
	t.Helper()
	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: stagesv1.StageSetSpec{
			Interval:           metav1.Duration{Duration: 5 * time.Minute},
			ServiceAccountName: sa,
			Stages:             []stagesv1.Stage{{Name: "stage-a", SourceRef: stagesv1.SourceReference{Name: eaName}}},
		},
	}
	mustCreate(t, c, ss)
	return ss
}

// A StageSet with spec.serviceAccountName applies under the impersonated SA:
// when the SA holds the needed RBAC, the apply succeeds.
func TestReconcile_Impersonation_AppliesUnderGrantedSA(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	grantConfigMaps(t, c, ns, "deployer")
	servedArtifact(t, c, ns, "cm-art", "", map[string]string{"cm.yaml": configMapManifest(ns, "by-tenant")})

	ss := impersonatingStageSet(t, c, ns, "ok", "deployer", "cm-art")
	reconcileWithConfig(t, c, ss)

	if r := readyReason(getStageSet(t, c, ns, "ok")); r != ReasonReady {
		t.Fatalf("Ready reason = %q, want %q", r, ReasonReady)
	}
	if !cmExists(t, c, ns, "by-tenant") {
		t.Fatal("ConfigMap should have been applied under the impersonated SA")
	}
}

// The impersonated SA's RBAC is the ceiling: an object the SA cannot create is
// rejected by the apiserver, the stage fails, and nothing is applied — proving
// the apply really runs as the SA, not the controller's broader identity.
func TestReconcile_Impersonation_DeniedBeyondSARBAC(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	// SA can touch ConfigMaps but not Secrets.
	grantConfigMaps(t, c, ns, "deployer")
	servedArtifact(t, c, ns, "secret-art", "", map[string]string{"s.yaml": secretManifest(ns, "forbidden")})

	ss := impersonatingStageSet(t, c, ns, "denied", "deployer", "secret-art")
	reconcileWithConfig(t, c, ss)

	if r := readyReason(getStageSet(t, c, ns, "denied")); r != ReasonStageFailed {
		t.Fatalf("Ready reason = %q, want %q", r, ReasonStageFailed)
	}
	var sec corev1.Secret
	err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "forbidden"}, &sec)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("Secret should not exist (apply was denied), get err = %v", err)
	}
}

// With no serviceAccountName the apply runs under the controller's own client —
// the single-tenant default — so it is unaffected by the tenant RBAC above.
func TestReconcile_Impersonation_EmptySAUsesControllerClient(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	servedArtifact(t, c, ns, "secret-art", "", map[string]string{"s.yaml": secretManifest(ns, "by-controller")})

	ss := impersonatingStageSet(t, c, ns, "default", "", "secret-art")
	reconcileWithConfig(t, c, ss)

	if r := readyReason(getStageSet(t, c, ns, "default")); r != ReasonReady {
		t.Fatalf("Ready reason = %q, want %q", r, ReasonReady)
	}
	var sec corev1.Secret
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "by-controller"}, &sec); err != nil {
		t.Fatalf("Secret should have been applied by the controller client: %v", err)
	}
}
