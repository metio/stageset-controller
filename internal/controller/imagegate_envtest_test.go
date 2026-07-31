// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/artifact"
)

// fakeImageVerifier stands in for the sigstore-go verifier: it returns a fixed
// resolved digest on success, or an error to reject the image.
type fakeImageVerifier struct {
	digest string
	err    error
	calls  int
}

func (f *fakeImageVerifier) Verify(_ context.Context, _ string, _ []stagesv1.VerificationAuthority, _ []stagesv1.AttestationRequirement) (string, error) {
	f.calls++
	if f.err != nil {
		return "", f.err
	}
	return f.digest, nil
}

func deploymentManifest(ns, name, image string) string {
	return fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
  namespace: %s
spec:
  selector:
    matchLabels: { app: %s }
  template:
    metadata:
      labels: { app: %s }
    spec:
      containers:
        - name: app
          image: %s
`, name, ns, name, name, image)
}

func imagePolicy(t *testing.T, c client.Client, name string, imageGlob string) {
	t.Helper()
	imagePolicyWithSkip(t, c, name, imageGlob, nil)
}

func imagePolicyWithSkip(t *testing.T, c client.Client, name, imageGlob string, skip []string) {
	t.Helper()
	p := &stagesv1.ImageVerificationPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: stagesv1.ImageVerificationPolicySpec{
			Images:      []string{imageGlob},
			Skip:        skip,
			Authorities: []stagesv1.VerificationAuthority{{Keyless: &stagesv1.KeylessAuthority{Issuer: "https://ci", Subject: "builder"}}},
		},
	}
	if err := c.Create(context.Background(), p); err != nil {
		t.Fatalf("create ImageVerificationPolicy: %v", err)
	}
	t.Cleanup(func() { _ = c.Delete(context.Background(), p) })
}

// TestReconcile_ImageVerification_SkipIsPerPolicy proves a Skip in one policy
// does not exempt the image from a second policy that governs it. Policies are
// cluster-scoped, so an author adding a narrow exemption to their own policy must
// not be able to disarm someone else's enforcement over the same image.
func TestReconcile_ImageVerification_SkipIsPerPolicy(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	// One broad policy that exempts the base images, one strict policy that
	// governs exactly those base images.
	imagePolicyWithSkip(t, c, "skip-broad", "regskip.io/**", []string{"regskip.io/base/**"})
	imagePolicy(t, c, "skip-strict", "regskip.io/base/**")

	servedArtifact(t, c, ns, "ea", "", map[string]string{
		"deploy.yaml": deploymentManifest(ns, "app", "regskip.io/base/distroless:1.0"),
	})
	ss := newStageSetNoWait(t, c, ns, "skipapp", stagesv1.SourceReference{Name: "ea"})

	v := &fakeImageVerifier{err: fmt.Errorf("no signature")}
	reconcileWithImageVerifier(t, c, ss, v, false)

	if v.calls == 0 {
		t.Fatal("the strict policy governs this image, so it must still be verified despite the broad policy's skip")
	}
	if r := readyReason(getStageSet(t, c, ns, "skipapp")); r != ReasonImageUnverified {
		t.Fatalf("Ready reason = %q, want %q", r, ReasonImageUnverified)
	}
	if _, applied := deployedImage(t, c, ns, "app"); applied {
		t.Fatal("an image rejected by a governing policy must not be applied")
	}
}

// newStageSetNoWait creates a single-stage StageSet whose stage opts out of the
// kstatus wait. envtest runs no kubelet, so an applied Deployment never reports a
// healthy status and the post-apply wait would block the whole verify timeout.
// The image-verification gate runs *before* apply, so skipping the wait leaves
// exactly what these tests exercise intact.
func newStageSetNoWait(t *testing.T, c client.Client, ns, name string, ref stagesv1.SourceReference) *stagesv1.StageSet {
	t.Helper()
	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: stagesv1.StageSetSpec{
			Interval: metav1.Duration{Duration: 5 * time.Minute},
			Stages: []stagesv1.Stage{{
				Name:        "stage-a",
				SourceRef:   ref,
				ReadyChecks: &stagesv1.ReadyChecks{DisableWait: true},
			}},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	return ss
}

func reconcileWithImageVerifier(t *testing.T, c client.Client, ss *stagesv1.StageSet, v *fakeImageVerifier, require bool) {
	t.Helper()
	r := &StageSetReconciler{
		Client:                   c,
		RESTMapper:               c.RESTMapper(),
		Fetcher:                  &artifact.Fetcher{HTTPClient: http.DefaultClient, URLValidator: artifact.PermissiveHTTPURL, IPValidator: artifact.PermissiveIP},
		ImageVerifier:            v,
		RequireImageVerification: require,
	}
	if _, err := driveReconcile(r, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ss.Namespace, Name: ss.Name}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
}

// TestReconcile_ImageVerification_GatesRollback proves a rollback cannot land an
// image the forward apply would have refused. The producer re-fetch path rebuilds
// from the artifact, so the previous revision's images arrive unverified and
// unpinned; verifying them keeps the "a stage never applies an unverified image"
// invariant true on the way back as well as the way forward.
func TestReconcile_ImageVerification_GatesRollback(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	imagePolicy(t, c, "rb-pol", "regrb.io/**")
	servedArtifact(t, c, ns, "ea-a", "", map[string]string{"deploy.yaml": deploymentManifest(ns, "app", "regrb.io/app:1.0")})
	servedArtifact(t, c, ns, "ea-b", "", map[string]string{"cm.yaml": configMapManifest(ns, "obj-b")})

	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "rb-verify"},
		Spec: stagesv1.StageSetSpec{
			Interval:          metav1.Duration{Duration: 5 * time.Minute},
			RollbackOnFailure: true,
			Stages: []stagesv1.Stage{
				{Name: "stage-a", SourceRef: stagesv1.SourceReference{Name: "ea-a"}, ReadyChecks: &stagesv1.ReadyChecks{DisableWait: true}},
				{Name: "stage-b", SourceRef: stagesv1.SourceReference{Name: "ea-b"}, ReadyChecks: &stagesv1.ReadyChecks{DisableWait: true}},
			},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}

	// A good run: the image verifies, so a rollback snapshot is recorded.
	good := &fakeImageVerifier{digest: "regrb.io/app@sha256:" + strings.Repeat("a", 64)}
	reconcileWithImageVerifier(t, c, ss, good, false)
	if r := readyReason(getStageSet(t, c, ns, "rb-verify")); r != ReasonReady {
		t.Fatalf("first run Ready reason = %q, want %q", r, ReasonReady)
	}

	// Stage B now renders a manifest the apiserver rejects, failing the run after
	// stage A applied — and the image no longer verifies, so the revert of stage A
	// must be refused rather than re-applying an unverified image.
	repointArtifact(t, c, ns, "ea-b", map[string]string{"cm.yaml": cmValManifest(ns, "Bad_Name", "x")})
	reconcileWithImageVerifier(t, c, ss, &fakeImageVerifier{err: fmt.Errorf("signature revoked")}, false)

	if r := readyReason(getStageSet(t, c, ns, "rb-verify")); r != ReasonImageUnverified {
		t.Fatalf("Ready reason = %q, want %q — a rollback whose images fail verification must be refused", r, ReasonImageUnverified)
	}
}

func deployedImage(t *testing.T, c client.Client, ns, name string) (string, bool) {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"})
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, u); err != nil {
		return "", false
	}
	containers, _, _ := unstructured.NestedSlice(u.Object, "spec", "template", "spec", "containers")
	if len(containers) == 0 {
		return "", true
	}
	img, _, _ := unstructured.NestedString(containers[0].(map[string]any), "image")
	return img, true
}

// TestReconcile_ImageVerification_HoldsUnverified proves a stage whose image fails
// its policy is held under ImageUnverified and never applied.
func TestReconcile_ImageVerification_HoldsUnverified(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	imagePolicy(t, c, "hold-pol", "reghold.io/**")
	servedArtifact(t, c, ns, "ea", "", map[string]string{"deploy.yaml": deploymentManifest(ns, "app", "reghold.io/app:1.0")})
	ss := newStageSetNoWait(t, c, ns, "app", stagesv1.SourceReference{Name: "ea"})

	reconcileWithImageVerifier(t, c, ss, &fakeImageVerifier{err: fmt.Errorf("no signature")}, false)

	if r := readyReason(getStageSet(t, c, ns, "app")); r != ReasonImageUnverified {
		t.Fatalf("Ready reason = %q, want %q", r, ReasonImageUnverified)
	}
	if _, ok := deployedImage(t, c, ns, "app"); ok {
		t.Fatal("an unverified image must not be applied")
	}
}

// TestReconcile_ImageVerification_PinsVerified proves a verified image is applied and
// pinned to the digest the verifier resolved.
func TestReconcile_ImageVerification_PinsVerified(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	imagePolicy(t, c, "pin-pol", "regpin.io/**")
	servedArtifact(t, c, ns, "ea", "", map[string]string{"deploy.yaml": deploymentManifest(ns, "app", "regpin.io/app:1.0")})
	ss := newStageSetNoWait(t, c, ns, "app", stagesv1.SourceReference{Name: "ea"})

	reconcileWithImageVerifier(t, c, ss, &fakeImageVerifier{digest: "regpin.io/app@sha256:abc"}, false)

	if r := readyReason(getStageSet(t, c, ns, "app")); r != ReasonReady {
		t.Fatalf("Ready reason = %q, want %q", r, ReasonReady)
	}
	img, ok := deployedImage(t, c, ns, "app")
	if !ok {
		t.Fatal("a verified image should be applied")
	}
	if img != "regpin.io/app@sha256:abc" {
		t.Fatalf("applied image = %q, want it pinned to the verified digest", img)
	}
}

// TestReconcile_ImageVerification_DenyByDefault proves --require-image-verification
// holds an image that matches no policy.
func TestReconcile_ImageVerification_DenyByDefault(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	// No policy governs regdeny.io.
	servedArtifact(t, c, ns, "ea", "", map[string]string{"deploy.yaml": deploymentManifest(ns, "app", "regdeny.io/app:1.0")})
	ss := newStageSetNoWait(t, c, ns, "app", stagesv1.SourceReference{Name: "ea"})

	reconcileWithImageVerifier(t, c, ss, &fakeImageVerifier{digest: "unused"}, true)

	if r := readyReason(getStageSet(t, c, ns, "app")); r != ReasonImageUnverified {
		t.Fatalf("deny-by-default should hold an ungoverned image; reason = %q", r)
	}
}

// TestReconcile_ImageVerification_BreakGlass proves the break-glass annotation
// applies a stage despite a failing verifier, and does not call it.
func TestReconcile_ImageVerification_BreakGlass(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	imagePolicy(t, c, "bg-pol", "regbg.io/**")
	servedArtifact(t, c, ns, "ea", "", map[string]string{"deploy.yaml": deploymentManifest(ns, "app", "regbg.io/app:1.0")})
	ss := newStageSetNoWait(t, c, ns, "app", stagesv1.SourceReference{Name: "ea"})
	ss.Annotations = map[string]string{skipImageVerificationAnnotation: "pipeline outage, INC-1234"}
	if err := c.Update(context.Background(), ss); err != nil {
		t.Fatalf("annotate StageSet: %v", err)
	}

	v := &fakeImageVerifier{err: fmt.Errorf("no signature")}
	reconcileWithImageVerifier(t, c, ss, v, true)

	if r := readyReason(getStageSet(t, c, ns, "app")); r != ReasonReady {
		t.Fatalf("break-glass should apply the stage; reason = %q", r)
	}
	if v.calls != 0 {
		t.Fatalf("break-glass must not verify; verifier was called %d times", v.calls)
	}
	if img, ok := deployedImage(t, c, ns, "app"); !ok || img != "regbg.io/app:1.0" {
		t.Fatalf("break-glass applies the image unpinned; got %q, ok=%v", img, ok)
	}
}

// TestReconcile_ImageVerification_SkipExempts proves a policy Skip glob exempts an
// image: it applies unverified (and unpinned) rather than being held.
func TestReconcile_ImageVerification_SkipExempts(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)
	p := &stagesv1.ImageVerificationPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "skip-pol"},
		Spec: stagesv1.ImageVerificationPolicySpec{
			Images:      []string{"regskip.io/**"},
			Skip:        []string{"regskip.io/vendored/**"},
			Authorities: []stagesv1.VerificationAuthority{{Keyless: &stagesv1.KeylessAuthority{Issuer: "https://ci", Subject: "builder"}}},
		},
	}
	if err := c.Create(context.Background(), p); err != nil {
		t.Fatalf("create policy: %v", err)
	}
	t.Cleanup(func() { _ = c.Delete(context.Background(), p) })

	servedArtifact(t, c, ns, "ea", "", map[string]string{"deploy.yaml": deploymentManifest(ns, "app", "regskip.io/vendored/base:1.0")})
	ss := newStageSetNoWait(t, c, ns, "app", stagesv1.SourceReference{Name: "ea"})

	v := &fakeImageVerifier{err: fmt.Errorf("no signature")}
	reconcileWithImageVerifier(t, c, ss, v, true)

	if r := readyReason(getStageSet(t, c, ns, "app")); r != ReasonReady {
		t.Fatalf("a skipped image should apply; reason = %q", r)
	}
	if v.calls != 0 {
		t.Fatalf("a skipped image must not be verified; verifier was called %d times", v.calls)
	}
}
