// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package observability

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace/noop"
)

// InitTracer with a mid-range SampleRatio takes the TraceIDRatioBased branch of
// samplerFor while still building the full exporter/resource/provider chain.
// otlptrace dials lazily, so a dead address still constructs successfully.
func TestInitTracer_MidRatioBuildsRatioSampler(t *testing.T) {
	t.Cleanup(func() { otel.SetTracerProvider(noop.NewTracerProvider()) })
	sd, err := InitTracer(context.Background(), TracingConfig{
		Endpoint:       "127.0.0.1:4317",
		Insecure:       true,
		ServiceName:    "explicit-name",
		ServiceVersion: "v9",
		SampleRatio:    0.5,
	})
	if err != nil {
		t.Fatalf("InitTracer: %v", err)
	}
	if sd == nil {
		t.Fatal("nil shutdown func")
	}
	_ = sd(context.Background())
}

// Shutting down the provider built against a dead collector with an
// already-cancelled context exercises the errors.Join branch where both the
// tracer-provider and exporter shutdowns can report an error.
func TestInitTracer_ShutdownJoinsErrorsOnDeadCollector(t *testing.T) {
	t.Cleanup(func() { otel.SetTracerProvider(noop.NewTracerProvider()) })
	sd, err := InitTracer(context.Background(), TracingConfig{
		Endpoint:    "127.0.0.1:4317",
		Insecure:    true,
		SampleRatio: 1.0,
	})
	if err != nil {
		t.Fatalf("InitTracer: %v", err)
	}

	// Emit a span so the batch processor has work to flush, then shut down under a
	// deadline that has already elapsed. The shutdown must not panic; any returned
	// error is the joined tracer-provider/exporter error and is acceptable.
	_, span := Tracer().Start(context.Background(), "pending")
	span.End()

	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()
	_ = sd(ctx)
}

// The empty-endpoint no-op path installs a provider whose Tracer is usable and
// whose shutdown is a nil-returning no-op even under a cancelled context.
func TestInitTracer_NoopShutdownIgnoresCancelledContext(t *testing.T) {
	sd, err := InitTracer(context.Background(), TracingConfig{})
	if err != nil {
		t.Fatalf("InitTracer: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sd(ctx); err != nil {
		t.Fatalf("no-op shutdown returned %v, want nil", err)
	}
}
