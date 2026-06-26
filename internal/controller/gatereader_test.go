// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestGateReader(t *testing.T) {
	cached := fake.NewClientBuilder().WithScheme(builderScheme(t)).Build()
	uncached := fake.NewClientBuilder().WithScheme(builderScheme(t)).Build()
	other := fake.NewClientBuilder().WithScheme(builderScheme(t)).Build()

	t.Run("controller's cached client reads via the uncached APIReader", func(t *testing.T) {
		r := &StageSetReconciler{Client: cached, APIReader: uncached}
		if got := r.gateReader(cached); got != client.Reader(uncached) {
			t.Fatal("expected the uncached APIReader for the controller's cached client")
		}
	})

	t.Run("an impersonated/remote target is read directly", func(t *testing.T) {
		r := &StageSetReconciler{Client: cached, APIReader: uncached}
		if got := r.gateReader(other); got != client.Reader(other) {
			t.Fatal("expected the target client for a non-controller target")
		}
	})

	t.Run("nil APIReader falls back to the target", func(t *testing.T) {
		r := &StageSetReconciler{Client: cached}
		if got := r.gateReader(cached); got != client.Reader(cached) {
			t.Fatal("expected fallback to the target when APIReader is nil")
		}
	})
}
