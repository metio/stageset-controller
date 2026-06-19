// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"net/http"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/artifact"
)

// reconcileOnceTraced reconciles a StageSet once with an in-memory span
// recorder installed as the global tracer provider, returning the spans the
// run produced. The provider is restored on cleanup so other tests keep the
// no-op tracer.
func reconcileOnceTraced(t *testing.T, c client.Client, ss *stagesv1.StageSet) []sdktrace.ReadOnlySpan {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetTracerProvider(prev)
		_ = tp.Shutdown(context.Background())
	})

	r := &StageSetReconciler{
		Client:     c,
		RESTMapper: c.RESTMapper(),
		Fetcher:    &artifact.Fetcher{HTTPClient: http.DefaultClient, URLValidator: artifact.PermissiveHTTPURL, IPValidator: artifact.PermissiveIP},
	}
	if _, err := driveReconcile(r, ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: ss.Namespace, Name: ss.Name},
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if err := tp.ForceFlush(context.Background()); err != nil {
		t.Fatalf("flush spans: %v", err)
	}
	return sr.Ended()
}

// A successful reconcile emits the root span plus the per-phase child spans,
// and — critically — still flips Ready=true. Spans are additive: they must
// never displace the Ready condition the rest of the suite relies on.
func TestReconcile_Instrumentation_EmitsSpansAndPreservesReady(t *testing.T) {
	c := testClient(t)
	ns := newNamespace(t, c)

	servedArtifact(t, c, ns, "dashboards-artifact", "dashboards",
		map[string]string{"cm.yaml": configMapManifest(ns, "deployed")})

	ss := newStageSet(t, c, ns, "platform", stagesv1.SourceReference{
		APIVersion: "jaas.metio.wtf/v1",
		Kind:       "JsonnetSnippet",
		Name:       "dashboards",
	})

	spans := reconcileOnceTraced(t, c, ss)

	// Conditions survive instrumentation: Ready must still be true.
	got := getStageSet(t, c, ns, "platform")
	if r := readyReason(got); r != ReasonReady {
		t.Fatalf("Ready reason = %q, want %q (instrumentation must not weaken the condition)", r, ReasonReady)
	}

	names := map[string]bool{}
	for _, s := range spans {
		names[s.Name()] = true
	}
	// The root span plus the genuine per-run phases a single-stage success
	// crosses. gateWindows/planMigrations/buildDecryptor run once per reconcile;
	// fetch/build/apply run per stage. decrypt is skipped here (no decryption).
	for _, want := range []string{
		"StageSet.Reconcile",
		"stageset.gateWindows",
		"stageset.planMigrations",
		"stageset.buildDecryptor",
		"stage.fetch",
		"stage.build",
		"stage.apply",
	} {
		if !names[want] {
			t.Errorf("missing span %q; got %v", want, names)
		}
	}
}
