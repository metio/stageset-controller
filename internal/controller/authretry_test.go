// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	fluxmeta "github.com/fluxcd/pkg/apis/meta"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

// countingClient answers every call with errs[n] for the n-th call, repeating
// the last entry once exhausted. The embedded nil client.Client is never
// reached: the tests only exercise the methods overridden here.
type countingClient struct {
	client.Client
	errs  []error
	calls int
}

func (c *countingClient) next() error {
	err := c.errs[min(c.calls, len(c.errs)-1)]
	c.calls++
	return err
}

func (c *countingClient) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return c.next()
}

func (c *countingClient) Patch(context.Context, client.Object, client.Patch, ...client.PatchOption) error {
	return c.next()
}

func (c *countingClient) SubResource(string) client.SubResourceClient {
	return &countingSubResource{parent: c}
}

type countingSubResource struct {
	client.SubResourceClient
	parent *countingClient
}

func (s *countingSubResource) Patch(context.Context, client.Object, client.Patch, ...client.SubResourcePatchOption) error {
	return s.parent.next()
}

func unauthorized() error {
	return apierrors.NewUnauthorized("Unauthorized")
}

// refreshTo builds a refresh func handing out the supplied clients in order and
// counting how often it was asked.
func refreshTo(calls *int, clients ...client.Client) func(context.Context) (client.Client, error) {
	return func(context.Context) (client.Client, error) {
		c := clients[min(*calls, len(clients)-1)]
		*calls++
		return c, nil
	}
}

func TestRetryUnauthorizedClient_RetriesOnceWithFreshCredential(t *testing.T) {
	stale := &countingClient{errs: []error{unauthorized()}}
	fresh := &countingClient{errs: []error{nil}}
	refreshes := 0
	c := newRetryUnauthorizedClient(stale, "ns", "sa", refreshTo(&refreshes, fresh))

	if err := c.Get(context.Background(), client.ObjectKey{}, &corev1.ConfigMap{}); err != nil {
		t.Fatalf("Get after re-mint: %v", err)
	}
	if refreshes != 1 {
		t.Errorf("refreshes = %d, want 1", refreshes)
	}
	if stale.calls != 1 || fresh.calls != 1 {
		t.Errorf("calls: stale = %d fresh = %d, want 1 and 1", stale.calls, fresh.calls)
	}
}

// Once the credential has been refreshed, later calls go straight to the new
// client — a single 401 must not cost a re-mint per call for the rest of the run.
func TestRetryUnauthorizedClient_KeepsRefreshedClientForLaterCalls(t *testing.T) {
	stale := &countingClient{errs: []error{unauthorized()}}
	fresh := &countingClient{errs: []error{nil}}
	refreshes := 0
	c := newRetryUnauthorizedClient(stale, "ns", "sa", refreshTo(&refreshes, fresh))

	for range 3 {
		if err := c.Get(context.Background(), client.ObjectKey{}, &corev1.ConfigMap{}); err != nil {
			t.Fatalf("Get: %v", err)
		}
	}
	if refreshes != 1 {
		t.Errorf("refreshes = %d, want 1", refreshes)
	}
	if stale.calls != 1 {
		t.Errorf("stale client calls = %d, want 1", stale.calls)
	}
	if fresh.calls != 3 {
		t.Errorf("refreshed client calls = %d, want 3", fresh.calls)
	}
}

func TestRetryUnauthorizedClient_SecondUnauthorizedSurfacesWithExplanation(t *testing.T) {
	stale := &countingClient{errs: []error{unauthorized()}}
	fresh := &countingClient{errs: []error{unauthorized()}}
	refreshes := 0
	c := newRetryUnauthorizedClient(stale, "tenant", "deployer", refreshTo(&refreshes, fresh))

	err := c.Get(context.Background(), client.ObjectKey{}, &corev1.ConfigMap{})
	if !apierrors.IsUnauthorized(err) {
		t.Fatalf("error = %v, want an Unauthorized", err)
	}
	if refreshes != 1 {
		t.Errorf("refreshes = %d, want exactly 1 (the retry must be bounded)", refreshes)
	}
	if !strings.Contains(err.Error(), "tenant/deployer") {
		t.Errorf("message %q does not name the ServiceAccount", err)
	}
}

func TestRetryUnauthorizedClient_OtherErrorsAreNotRetried(t *testing.T) {
	notFound := apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, "cm")
	stale := &countingClient{errs: []error{notFound}}
	refreshes := 0
	c := newRetryUnauthorizedClient(stale, "ns", "sa", refreshTo(&refreshes, stale))

	err := c.Get(context.Background(), client.ObjectKey{}, &corev1.ConfigMap{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("error = %v, want the NotFound passed through", err)
	}
	if refreshes != 0 {
		t.Errorf("refreshes = %d, want 0", refreshes)
	}
}

// A failed re-mint must keep the 401 at the head of the chain: callers classify
// on it, and a wrapper that swallowed it would turn an authentication failure
// into an unrecognisable minting failure.
func TestRetryUnauthorizedClient_RefreshFailurePreservesUnauthorized(t *testing.T) {
	stale := &countingClient{errs: []error{unauthorized()}}
	c := newRetryUnauthorizedClient(stale, "ns", "sa", func(context.Context) (client.Client, error) {
		return nil, errors.New("TokenRequest refused")
	})

	err := c.Get(context.Background(), client.ObjectKey{}, &corev1.ConfigMap{})
	if !apierrors.IsUnauthorized(err) {
		t.Fatalf("error = %v, want an Unauthorized", err)
	}
	if !strings.Contains(err.Error(), "TokenRequest refused") {
		t.Errorf("message %q drops the minting failure", err)
	}
}

func TestRetryUnauthorizedClient_SubresourceCallsRetryToo(t *testing.T) {
	stale := &countingClient{errs: []error{unauthorized()}}
	fresh := &countingClient{errs: []error{nil}}
	refreshes := 0
	c := newRetryUnauthorizedClient(stale, "ns", "sa", refreshTo(&refreshes, fresh))

	err := c.Status().Patch(context.Background(), &corev1.ConfigMap{}, client.Merge)
	if err != nil {
		t.Fatalf("status patch after re-mint: %v", err)
	}
	if refreshes != 1 {
		t.Errorf("refreshes = %d, want 1", refreshes)
	}
}

// The mutation methods share do, but a 401 on an apply is the reported symptom,
// so pin that path explicitly.
func TestRetryUnauthorizedClient_PatchRetries(t *testing.T) {
	stale := &countingClient{errs: []error{unauthorized()}}
	fresh := &countingClient{errs: []error{nil}}
	refreshes := 0
	c := newRetryUnauthorizedClient(stale, "ns", "sa", refreshTo(&refreshes, fresh))

	if err := c.Patch(context.Background(), &corev1.ConfigMap{}, client.Merge); err != nil {
		t.Fatalf("Patch after re-mint: %v", err)
	}
	if refreshes != 1 {
		t.Errorf("refreshes = %d, want 1", refreshes)
	}
}

func TestForgetTenant_DropsTokenAndConnection(t *testing.T) {
	fm := &fakeMinter{token: "t1", expires: time.Now().Add(time.Hour)}
	r := &StageSetReconciler{
		tokens:  newTokenCache(fm),
		targets: map[string]clusterTarget{localTargetKey("ns", "sa"): {token: "t1"}},
	}
	if _, err := r.tokens.Token(context.Background(), "ns", "sa"); err != nil {
		t.Fatalf("Token: %v", err)
	}

	r.forgetTenant("ns", "sa")

	if _, ok := r.targets[localTargetKey("ns", "sa")]; ok {
		t.Error("target connection survived forgetTenant")
	}
	if _, err := r.tokens.Token(context.Background(), "ns", "sa"); err != nil {
		t.Fatalf("Token: %v", err)
	}
	if fm.callCount() != 2 {
		t.Errorf("mint calls = %d, want 2 (forgetTenant must force a re-mint)", fm.callCount())
	}
}

// mintsTokenFor decides which targets are wrapped. Only the local impersonated
// path carries a credential this controller can re-mint.
func TestMintsTokenFor(t *testing.T) {
	remote := &fluxmeta.KubeConfigReference{SecretRef: &fluxmeta.SecretKeyReference{Name: "kubeconfig"}}
	for _, tc := range []struct {
		name              string
		skipImpersonation bool
		sa                string
		kc                *fluxmeta.KubeConfigReference
		want              bool
	}{
		{name: "local tenant SA", sa: "deployer", want: true},
		{name: "no tenant SA", sa: "", want: false},
		{name: "remote target", sa: "deployer", kc: remote, want: false},
		{name: "impersonation skipped", skipImpersonation: true, sa: "deployer", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &StageSetReconciler{SkipImpersonation: tc.skipImpersonation}
			if got := r.mintsTokenFor(tc.sa, tc.kc); got != tc.want {
				t.Errorf("mintsTokenFor(%q, %v) = %v, want %v", tc.sa, tc.kc, got, tc.want)
			}
		})
	}
}

func TestForgetDeletedTenants_EvictsUnsharedServiceAccounts(t *testing.T) {
	scheme := watchScheme(t)
	going := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "tenant", Name: "going"},
		Spec: stagesv1.StageSetSpec{
			ServiceAccountName: "shared",
			Stages:             []stagesv1.Stage{{Name: "one", ServiceAccountName: "solo"}},
		},
	}
	staying := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "tenant", Name: "staying"},
		Spec:       stagesv1.StageSetSpec{ServiceAccountName: "shared"},
	}
	fm := &fakeMinter{token: "t1", expires: time.Now().Add(time.Hour)}
	r := &StageSetReconciler{
		Client:  fake.NewClientBuilder().WithScheme(scheme).WithObjects(going, staying).Build(),
		tokens:  newTokenCache(fm),
		targets: map[string]clusterTarget{},
	}
	for _, sa := range []string{"shared", "solo"} {
		r.targets[localTargetKey("tenant", sa)] = clusterTarget{token: "t1"}
	}

	r.forgetDeletedTenants(context.Background(), going)

	if _, ok := r.targets[localTargetKey("tenant", "solo")]; ok {
		t.Error("solo credential survived; no other StageSet uses it")
	}
	if _, ok := r.targets[localTargetKey("tenant", "shared")]; !ok {
		t.Error("shared credential evicted; the staying StageSet still uses it")
	}
}

func TestForgetDeletedTenants_IgnoresStageSetsAlreadyDeleting(t *testing.T) {
	scheme := watchScheme(t)
	deleting := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "tenant", Name: "also-going",
			DeletionTimestamp: &metav1.Time{Time: time.Now()},
			Finalizers:        []string{FinalizerName},
		},
		Spec: stagesv1.StageSetSpec{ServiceAccountName: "shared"},
	}
	going := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "tenant", Name: "going"},
		Spec:       stagesv1.StageSetSpec{ServiceAccountName: "shared"},
	}
	r := &StageSetReconciler{
		Client:  fake.NewClientBuilder().WithScheme(scheme).WithObjects(deleting, going).Build(),
		tokens:  newTokenCache(&fakeMinter{token: "t1", expires: time.Now().Add(time.Hour)}),
		targets: map[string]clusterTarget{localTargetKey("tenant", "shared"): {token: "t1"}},
	}

	r.forgetDeletedTenants(context.Background(), going)

	if _, ok := r.targets[localTargetKey("tenant", "shared")]; ok {
		t.Error("credential survived; the only other user is itself being deleted")
	}
}
