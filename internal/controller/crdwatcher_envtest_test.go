/*
 * SPDX-FileCopyrightText: The stageset-controller Authors
 * SPDX-License-Identifier: 0BSD
 */

package controller

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// widgetGroupSeq gives each invocation of the CRD-watcher envtest a unique CRD
// group, so repeated runs against the shared apiserver don't collide on a
// Terminating CRD that envtest never finalizes away.
var widgetGroupSeq atomic.Uint64

// TestEnvtest_CRDWatcher_LateInstallEngagesProducerWatchLive installs a producer
// CRD that a StageSet references AFTER the manager and CRDWatcher are up. The
// watcher observes the Established transition and engages the producer watch
// live — the GVK leaves MissingProducerKinds — without a process restart. It
// exercises the full chain: informer → handleCRD → EngageProducerWatch →
// controller.Watch against a freshly-installed CRD (the real integration risk,
// since the manager's dynamic RESTMapper must reload discovery to map the new
// kind). The manager keeps running throughout.
func TestEnvtest_CRDWatcher_LateInstallEngagesProducerWatchLive(t *testing.T) {
	cfg := envtestConfig(t)
	scheme := testScheme(t)
	if err := apiextv1.AddToScheme(scheme); err != nil {
		t.Fatalf("register apiextv1: %v", err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("crd client: %v", err)
	}

	// A unique group per invocation: envtest's apiserver never clears the CRD's
	// cleanup finalizer (no finalizer controller runs), so a deleted CRD lingers
	// in Terminating. A fresh group keeps re-runs (go test -count=N, the shared
	// apiserver) independent — and a lingering CRD from a prior run is inert here,
	// since crdServesGVK matches on group and this run's missing GVK is a new one.
	seq := strconv.FormatUint(widgetGroupSeq.Add(1), 10)
	group := "crdwatch" + seq + ".example.com"
	crdName := "widgets." + group
	widgetGVK := schema.GroupVersionKind{Group: group, Version: "v1", Kind: "Widget"}

	// Precondition: the kind must be absent, or the missing→installed transition
	// under test never happens.
	if err := c.Get(context.Background(), client.ObjectKey{Name: crdName}, &apiextv1.CustomResourceDefinition{}); err == nil {
		t.Fatal("precondition: Widget CRD must not be pre-installed")
	}

	// SkipNameValidation: the "stageset" controller name is registered in a
	// process-global set, so repeated in-process runs (go test -count=N) would
	// otherwise collide on it.
	skipNameValidation := true
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		Controller:             ctrlconfig.Controller{SkipNameValidation: &skipNameValidation},
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	r := &StageSetReconciler{Client: mgr.GetClient(), SkipImpersonation: true}
	if err := r.SetupWithManager(mgr); err != nil {
		t.Fatalf("setup reconciler: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	mgrDone := make(chan error, 1)
	go func() { mgrDone <- mgr.Start(ctx) }()
	go func() {
		_ = (&CRDWatcher{
			RestCfg: cfg,
			Engager: r,
			Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		}).Start(ctx)
	}()

	if !mgr.GetCache().WaitForCacheSync(ctx) {
		t.Fatal("manager cache did not sync")
	}

	// Seed the missing set the way Reconcile does: engageProducerWatch is the
	// exact call it makes for every producer sourceRef. The kind isn't installed,
	// so it records as missing instead of engaging.
	r.engageProducerWatch(widgetGVK)
	if !slices.Contains(r.MissingProducerKinds(), widgetGVK) {
		t.Fatalf("Widget not recorded as missing after engageProducerWatch: %v", r.MissingProducerKinds())
	}

	widget := producerStubCRD("widgets", group, "Widget")
	if err := c.Create(context.Background(), widget); err != nil {
		t.Fatalf("create Widget CRD: %v", err)
	}
	// Best-effort delete; the unique group makes a lingering Terminating CRD inert
	// for other tests, so no GC wait is needed (envtest clears no finalizers).
	t.Cleanup(func() { _ = c.Delete(context.Background(), widget) })
	pollFor(t, 10*time.Second, func() (bool, string) {
		var got apiextv1.CustomResourceDefinition
		if err := c.Get(context.Background(), client.ObjectKey{Name: crdName}, &got); err != nil {
			return false, "get: " + err.Error()
		}
		got.Status.Conditions = []apiextv1.CustomResourceDefinitionCondition{
			{Type: apiextv1.Established, Status: apiextv1.ConditionTrue, Reason: "Test", Message: "patched by test"},
		}
		if err := c.Status().Update(context.Background(), &got); err != nil {
			return false, "establish: " + err.Error()
		}
		return true, ""
	})

	// The watcher engages the live watch and prunes Widget. Re-touch the CRD each
	// iteration so the watcher re-fires: the manager's dynamic RESTMapper may need
	// a discovery reload before controller.Watch can map the fresh kind, and the
	// watcher leans on this re-trigger (in production, the reconcile backstop)
	// rather than its own retry loop.
	touch := 0
	pollFor(t, 30*time.Second, func() (bool, string) {
		if !slices.Contains(r.MissingProducerKinds(), widgetGVK) {
			return true, ""
		}
		var got apiextv1.CustomResourceDefinition
		if err := c.Get(context.Background(), client.ObjectKey{Name: crdName}, &got); err == nil {
			if got.Annotations == nil {
				got.Annotations = map[string]string{}
			}
			touch++
			got.Annotations["crdwatch.example.com/touch"] = strconv.Itoa(touch)
			_ = c.Update(context.Background(), &got)
		}
		return false, "Widget still in MissingProducerKinds"
	})

	select {
	case err := <-mgrDone:
		t.Errorf("manager exited unexpectedly: %v", err)
	default:
	}
}

// pollFor calls cond until it reports success or timeout elapses, failing the
// test with the last reported reason.
func pollFor(t *testing.T, timeout time.Duration, cond func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		ok, msg := cond()
		if ok {
			return
		}
		last = msg
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("condition not met within %v: %s", timeout, last)
}

// producerStubCRD is a minimal namespaced CRD (open spec+status, status
// subresource) — enough for envtest to serve the kind so a source.Kind informer
// can list it.
func producerStubCRD(plural, group, kind string) *apiextv1.CustomResourceDefinition {
	preserve := true
	return &apiextv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: plural + "." + group},
		Spec: apiextv1.CustomResourceDefinitionSpec{
			Group: group,
			Names: apiextv1.CustomResourceDefinitionNames{
				Kind:     kind,
				ListKind: kind + "List",
				Plural:   plural,
				Singular: strings.ToLower(kind),
			},
			Scope: apiextv1.NamespaceScoped,
			Versions: []apiextv1.CustomResourceDefinitionVersion{{
				Name:    "v1",
				Served:  true,
				Storage: true,
				Subresources: &apiextv1.CustomResourceSubresources{
					Status: &apiextv1.CustomResourceSubresourceStatus{},
				},
				Schema: &apiextv1.CustomResourceValidation{
					OpenAPIV3Schema: &apiextv1.JSONSchemaProps{
						Type: "object",
						Properties: map[string]apiextv1.JSONSchemaProps{
							"spec":   {Type: "object", XPreserveUnknownFields: &preserve},
							"status": {Type: "object", XPreserveUnknownFields: &preserve},
						},
					},
				},
			}},
		},
	}
}
