// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

// The whole deletion path hangs on one apiserver behaviour the watch predicate
// depends on: deleting a finalizer-held object stamps deletionTimestamp AND
// bumps metadata.generation, so the resulting Update event survives
// GenerationChangedPredicate. Were it filtered, a deleted StageSet would keep
// its finalizer — and hold its namespace in Terminating — until the reconcile
// interval or an unrelated event happened to wake it. Only a real apiserver can
// answer this, so it lives behind envtest.
func TestStageSetWatchPredicate_WakesOnDeletion(t *testing.T) {
	cfg := envtestConfig(t)
	c, err := client.New(cfg, client.Options{Scheme: testScheme(t)})
	if err != nil {
		t.Fatalf("build client: %v", err)
	}
	ctx := context.Background()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "deletion-event"}}
	if err := c.Create(ctx, ns); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	ss := &stagesv1.StageSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:  ns.Name,
			Name:       "held",
			Finalizers: []string{FinalizerName},
		},
		Spec: stagesv1.StageSetSpec{
			Interval: metav1.Duration{Duration: time.Minute},
			Stages: []stagesv1.Stage{{
				Name:      "one",
				SourceRef: stagesv1.SourceReference{Name: "src"},
			}},
		},
	}
	if err := c.Create(ctx, ss); err != nil {
		t.Fatalf("create StageSet: %v", err)
	}
	before := ss.DeepCopy()

	if err := c.Delete(ctx, ss); err != nil {
		t.Fatalf("delete StageSet: %v", err)
	}
	after := &stagesv1.StageSet{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(before), after); err != nil {
		t.Fatalf("get after delete: %v", err)
	}

	if after.DeletionTimestamp.IsZero() {
		t.Fatal("the finalizer did not hold the StageSet; nothing to assert")
	}
	if after.Generation <= before.Generation {
		t.Errorf("generation %d -> %d: the apiserver no longer bumps it on deletion, so the watch predicate drops the event",
			before.Generation, after.Generation)
	}
	if !stageSetWatchPredicate().Update(event.UpdateEvent{ObjectOld: before, ObjectNew: after}) {
		t.Error("watch predicate dropped the deletion event; the StageSet would keep its finalizer until an unrelated event woke it")
	}

	after.Finalizers = nil
	if err := c.Update(ctx, after); err != nil {
		t.Fatalf("release finalizer: %v", err)
	}
}
