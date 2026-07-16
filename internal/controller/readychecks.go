// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/fluxcd/cli-utils/pkg/object"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
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
// addition to the ones the stage applied.
//
// The namespace an entry resolves to depends on its kind's scope, which only the
// target cluster's RESTMapper knows. A namespaced kind with an empty namespace
// defaults to the StageSet's (the NamespacedObjectKindReference local-reference
// convention). A cluster-scoped kind — CustomResourceDefinition, ClusterRole,
// Namespace, PersistentVolume, StorageClass — must carry an EMPTY namespace:
// kstatus matches an object by its full ObjMetadata, and a cluster-scoped object
// has no namespace, so a namespaced key can never match one and the wait reports
// Unknown until the stage's verify timeout. That path is the whole "operator
// installs CRDs, a later stage applies the CRs" ordering the checks exist for.
//
// mapper is the target cluster's, so a remote cluster's own scoping applies. A
// kind it cannot resolve (a CRD not installed yet — which a check on a CRD's own
// existence cannot have, but a check on a CR of it might) falls back to the
// namespaced default: the wait then reports it Unknown, which is the honest
// answer for a kind the cluster does not serve.
func readyCheckObjects(mapper apimeta.RESTMapper, ss *stagesv1.StageSet, stage *stagesv1.Stage) object.ObjMetadataSet {
	if stage.ReadyChecks == nil {
		return nil
	}
	var set object.ObjMetadataSet
	for _, ref := range stage.ReadyChecks.Checks {
		gv, _ := schema.ParseGroupVersion(ref.APIVersion)
		gk := schema.GroupKind{Group: gv.Group, Kind: ref.Kind}
		set = append(set, object.ObjMetadata{
			Namespace: readyCheckNamespace(mapper, gk, gv.Version, ref.Namespace, ss.Namespace),
			Name:      ref.Name,
			GroupKind: gk,
		})
	}
	return set
}

// readyCheckNamespace resolves the namespace an ObjMetadata for gk must carry.
// Cluster-scoped kinds get "" — including when the reference names one, since a
// namespace on a cluster-scoped object is meaningless rather than merely
// redundant, and honoring it would produce a key that matches nothing.
//
// Admission deliberately does NOT reject that spec. Scope is a property of the
// TARGET cluster, which for a spec.kubeConfig StageSet is not the one the
// webhook can see, and ValidateSpec is shared with the reconciler's
// bypass-admission fallback, which has no mapper at that point either. A rule
// built on the webhook's own mapper would be right only for local targets and
// quietly wrong for remote ones. The field's documentation states that the
// namespace is ignored for cluster-scoped kinds.
func readyCheckNamespace(mapper apimeta.RESTMapper, gk schema.GroupKind, version, refNamespace, ownerNamespace string) string {
	if clusterScoped(mapper, gk, version) {
		return ""
	}
	if refNamespace != "" {
		return refNamespace
	}
	return ownerNamespace
}

// clusterScoped reports whether gk is a root-scoped kind on the target cluster.
// An unresolvable kind reports false: the namespaced default is the safer guess
// for a CR whose CRD is not installed yet, and the check then simply stays
// Unknown rather than silently gating on a key nothing can occupy.
func clusterScoped(mapper apimeta.RESTMapper, gk schema.GroupKind, version string) bool {
	if mapper == nil {
		return false
	}
	var versions []string
	if version != "" {
		versions = []string{version}
	}
	m, err := mapper.RESTMapping(gk, versions...)
	if err != nil {
		return false
	}
	return m.Scope.Name() == apimeta.RESTScopeNameRoot
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
