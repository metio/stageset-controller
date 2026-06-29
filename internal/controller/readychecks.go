// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/fluxcd/cli-utils/pkg/object"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/celeval"
)

// readyCheckInterval is the poll cadence for CEL health checks, matching the
// kstatus wait's interval so the two gates feel uniform.
const readyCheckInterval = 2 * time.Second

// readyCheckObjects converts a stage's explicit ReadyChecks.Checks references
// into an ObjMetadataSet so the kstatus wait gates on those external objects in
// addition to the ones the stage applied. An empty namespace defaults to the
// StageSet's namespace (the NamespacedObjectKindReference local-reference
// convention).
func readyCheckObjects(ss *stagesv1.StageSet, stage *stagesv1.Stage) object.ObjMetadataSet {
	if stage.ReadyChecks == nil {
		return nil
	}
	var set object.ObjMetadataSet
	for _, ref := range stage.ReadyChecks.Checks {
		gv, _ := schema.ParseGroupVersion(ref.APIVersion)
		ns := ref.Namespace
		if ns == "" {
			ns = ss.Namespace
		}
		set = append(set, object.ObjMetadata{
			Namespace: ns,
			Name:      ref.Name,
			GroupKind: schema.GroupKind{Group: gv.Group, Kind: ref.Kind},
		})
	}
	return set
}

// compiledHealthCheck pairs a CustomHealthCheck with its compiled CEL programs.
type compiledHealthCheck struct {
	group, kind                 string
	current, inProgress, failed *celeval.Program
}

// compileReadyExprs compiles every CustomHealthCheck's CEL expressions. A
// malformed expression is a hard error — admission rejects these too, so this is
// the bypass-admission fallback.
func compileReadyExprs(stage *stagesv1.Stage) ([]compiledHealthCheck, error) {
	if stage.ReadyChecks == nil {
		return nil, nil
	}
	out := make([]compiledHealthCheck, 0, len(stage.ReadyChecks.Exprs))
	for i := range stage.ReadyChecks.Exprs {
		hc := &stage.ReadyChecks.Exprs[i]
		gv, err := schema.ParseGroupVersion(hc.APIVersion)
		if err != nil {
			return nil, fmt.Errorf("readyChecks.exprs[%d] apiVersion %q: %w", i, hc.APIVersion, err)
		}
		c := compiledHealthCheck{group: gv.Group, kind: hc.Kind}
		for name, expr := range map[string]**celeval.Program{"current": &c.current, "inProgress": &c.inProgress, "failed": &c.failed} {
			src := map[string]string{"current": hc.Current, "inProgress": hc.InProgress, "failed": hc.Failed}[name]
			if src == "" {
				continue
			}
			prog, err := celeval.Compile(src)
			if err != nil {
				return nil, fmt.Errorf("readyChecks.exprs[%d] %s %q: %w", i, name, src, err)
			}
			*expr = prog
		}
		out = append(out, c)
	}
	return out, nil
}

// evalReadyExprs polls the stage's CEL health checks until every Current
// expression holds (or a Failed expression trips, or the timeout elapses),
// matching the kstatus wait's gate-then-fail semantics so a not-yet-ready
// object keeps the stage waiting rather than failing it immediately. Each check
// is evaluated against the live state (on the target cluster) of the stage's
// applied objects whose group+kind matches; a check matching no applied object
// is a no-op (it gates only objects of that kind in the stage).
func evalReadyExprs(ctx context.Context, target client.Client, ss *stagesv1.StageSet, stage *stagesv1.Stage, applied []*unstructured.Unstructured, timeout time.Duration) error {
	checks, err := compileReadyExprs(stage)
	if err != nil {
		return err
	}
	if len(checks) == 0 {
		return nil
	}

	poll := func(ctx context.Context) (bool, error) {
		ready := true
		for i := range checks {
			c := &checks[i]
			for _, o := range applied {
				gvk := o.GroupVersionKind()
				if gvk.Group != c.group || gvk.Kind != c.kind {
					continue
				}
				live := &unstructured.Unstructured{}
				live.SetGroupVersionKind(gvk)
				key := client.ObjectKey{Namespace: o.GetNamespace(), Name: o.GetName()}
				if err := target.Get(ctx, key, live); err != nil {
					// The object isn't observable yet; keep waiting.
					ready = false
					continue
				}
				// A tripped Failed expression is terminal — stop polling.
				if c.failed != nil {
					if failed, ferr := c.failed.EvalBool(live.Object); ferr == nil && failed {
						return false, fmt.Errorf("readyCheck failed for %s %s/%s", c.kind, key.Namespace, key.Name)
					}
				}
				// Current must hold for the object to count as ready; an eval
				// error (e.g. status not yet populated) reads as not-ready.
				if c.current != nil {
					if cur, cerr := c.current.EvalBool(live.Object); cerr != nil || !cur {
						ready = false
					}
				}
			}
		}
		return ready, nil
	}

	if err := wait.PollUntilContextTimeout(ctx, readyCheckInterval, timeout, true, poll); err != nil {
		return fmt.Errorf("readyChecks.exprs not satisfied within %s: %w", timeout, err)
	}
	return nil
}
