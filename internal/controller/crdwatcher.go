/*
 * SPDX-FileCopyrightText: The stageset-controller Authors
 * SPDX-License-Identifier: 0BSD
 */

package controller

import (
	"context"
	"fmt"
	"log/slog"

	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apiextinformers "k8s.io/apiextensions-apiserver/pkg/client/informers/externalversions"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	toolscache "k8s.io/client-go/tools/cache"
)

// ProducerEngager is the CRDWatcher's view of the StageSet reconciler: the set
// of producer kinds waiting on their CRD, and the handoff that engages a watch
// once the CRD lands. *StageSetReconciler satisfies it; tests substitute a fake.
type ProducerEngager interface {
	// MissingProducerKinds returns the producer GVKs a StageSet references
	// whose CRDs aren't installed yet.
	MissingProducerKinds() []schema.GroupVersionKind
	// EngageProducerWatch wires a live watch on a now-installed producer kind.
	EngageProducerWatch(ctx context.Context, gvk schema.GroupVersionKind) error
}

// CRDWatcher subscribes to the cluster's CustomResourceDefinition stream and,
// when a producer kind a StageSet is waiting on becomes Established, engages its
// watch immediately via the reconciler — no process restart, and no waiting for
// the next reconcile of a referencing StageSet.
//
// It is deliberately leaner than a self-contained retry engine: the reconcile
// path already re-attempts engagement every time a StageSet references the kind
// (engageProducerWatch), so a one-off failure here is not permanent — the next
// reconcile is the backstop. This watcher's job is purely to collapse the
// detection latency from "up to one reconcile interval" to sub-second. The cost
// is `get/list/watch` on customresourcedefinitions.apiextensions.k8s.io, granted
// by the chart's cluster ClusterRole.
//
// It uses client-go's apiextensions informer directly rather than the manager
// cache so its lifetime is decoupled from the manager's other cache wiring, and
// so a cluster without the CRD RBAC degrades to a warning instead of stalling
// the shared cache.
type CRDWatcher struct {
	RestCfg *rest.Config
	Engager ProducerEngager
	Logger  *slog.Logger
}

// Start runs the watcher until ctx is canceled. Returns nil on cancellation
// (clean shutdown) and, deliberately, nil on a cache-sync failure too — see
// degradeOnSyncFailure.
func (w *CRDWatcher) Start(ctx context.Context) error {
	if w.Engager == nil {
		return fmt.Errorf("crd watcher: engager is required")
	}
	logger := w.Logger
	if logger == nil {
		logger = slog.Default()
	}

	client, err := apiextclient.NewForConfig(w.RestCfg)
	if err != nil {
		return fmt.Errorf("crd watcher: build apiext client: %w", err)
	}

	factory := apiextinformers.NewSharedInformerFactory(client, 0)
	informer := factory.Apiextensions().V1().CustomResourceDefinitions().Informer()

	if _, err := informer.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { w.handleCRD(ctx, obj) },
		UpdateFunc: func(_, obj any) { w.handleCRD(ctx, obj) },
	}); err != nil {
		return fmt.Errorf("crd watcher: add event handler: %w", err)
	}

	// The factory takes a stop-channel, not a context; bridge ctx into one.
	stopCh := make(chan struct{})
	go func() {
		<-ctx.Done()
		close(stopCh)
	}()
	factory.Start(stopCh)
	if !waitForCacheSync(stopCh, informer.HasSynced) {
		return w.degradeOnSyncFailure(ctx, logger)
	}

	<-ctx.Done()
	return nil
}

// handleCRD engages any missing producer watch that crd now serves. Invoked
// serially per informer event (client-go delivers on one goroutine), so it
// needs no locking of its own — each engage either succeeds (the GVK leaves
// MissingProducerKinds, so later events skip it) or fails (bumping the
// engagement metric inside the reconciler, retried on the next reconcile).
// EngageProducerWatch is itself idempotent under the reconciler's own lock.
// Exposed on the receiver so tests can drive the CRD→engage handoff without an
// informer.
func (w *CRDWatcher) handleCRD(ctx context.Context, obj any) {
	crd, ok := obj.(*apiextv1.CustomResourceDefinition)
	if !ok || !isCRDEstablished(crd) {
		return
	}
	logger := w.Logger
	if logger == nil {
		logger = slog.Default()
	}
	for _, gvk := range w.Engager.MissingProducerKinds() {
		if !crdServesGVK(crd, gvk) {
			continue
		}
		if err := w.Engager.EngageProducerWatch(ctx, gvk); err != nil {
			logger.WarnContext(ctx, "Failed to engage producer watch for newly-installed CRD; will retry on the next reconcile",
				slog.String("gvk", gvk.String()),
				slog.String("crd", crd.Name),
				slog.Any("error", err))
		}
	}
}

// waitForCacheSync is behind a package var so a unit test can drive the failure
// branch without standing up a never-syncing informer.
var waitForCacheSync = func(stopCh <-chan struct{}, hasSynced ...toolscache.InformerSynced) bool {
	return toolscache.WaitForCacheSync(stopCh, hasSynced...)
}

// degradeOnSyncFailure logs a warning and returns nil. The watcher exists to let
// the controller run in clusters that lack (or haven't yet granted RBAC for) the
// CRD stream and engage producer watches later. Returning an error would
// propagate out of Start and take down the whole manager — including the
// reconcilers that are working fine — defeating that purpose. The reconcile path
// still engages producer watches as StageSets reference installed kinds; only
// the sub-second CRD-install detection is lost until the process restarts.
func (w *CRDWatcher) degradeOnSyncFailure(ctx context.Context, logger *slog.Logger) error {
	logger.WarnContext(ctx, "CRD watcher could not sync (missing customresourcedefinitions RBAC?); proactive producer-watch engagement disabled, restart to retry")
	return nil
}

// crdServesGVK reports whether crd is Established and serves gvk — its group and
// kind under a served version. Matching by group+kind+served-version, rather
// than a fixed <plural>.<group> name map, is what lets the watcher handle the
// arbitrary producer kinds a StageSet's sourceRef may name.
func crdServesGVK(crd *apiextv1.CustomResourceDefinition, gvk schema.GroupVersionKind) bool {
	if crd.Spec.Group != gvk.Group || crd.Spec.Names.Kind != gvk.Kind {
		return false
	}
	if !isCRDEstablished(crd) {
		return false
	}
	for _, v := range crd.Spec.Versions {
		if v.Served && v.Name == gvk.Version {
			return true
		}
	}
	return false
}

// isCRDEstablished returns true when the apiserver has installed the CRD and
// registered its discovery endpoint. Only then can the source.Kind informer
// start a watch on its instances.
func isCRDEstablished(crd *apiextv1.CustomResourceDefinition) bool {
	for _, c := range crd.Status.Conditions {
		if c.Type == apiextv1.Established && c.Status == apiextv1.ConditionTrue {
			return true
		}
	}
	return false
}
