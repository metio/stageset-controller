// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

// Package artifact resolves a stage's source reference to a ready Flux
// ExternalArtifact (directly or through an RFC-0012 producer back-pointer)
// and fetches the referenced tarball with the source-controller digest and
// size-cap contract.
package artifact

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

const (
	externalArtifactGroup   = "source.toolkit.fluxcd.io"
	externalArtifactVersion = "v1"
	externalArtifactKind    = "ExternalArtifact"
)

// Sentinel errors. Callers map them onto Ready-condition reasons and decide
// transient (requeue) versus steady-state (report and wait for a watch event).
var (
	// ErrSourceNotReady reports that the ExternalArtifact exists but its
	// status.conditions[Ready] is not True yet. Transient: a watch on the
	// artifact re-triggers once the producer marks it Ready.
	ErrSourceNotReady = errors.New("source artifact not ready")

	// ErrArtifactMissing reports that a Ready ExternalArtifact has no usable
	// status.artifact (url/digest) yet.
	ErrArtifactMissing = errors.New("source has no status.artifact yet")

	// ErrArtifactNotFound reports that the reference (direct or producer
	// back-pointer) resolves to no ExternalArtifact.
	ErrArtifactNotFound = errors.New("referenced ExternalArtifact not found")

	// ErrAmbiguousProducer reports that a producer reference back-resolves to
	// more than one ExternalArtifact, so the choice is undefined.
	ErrAmbiguousProducer = errors.New("producer reference resolves to multiple ExternalArtifacts")

	// ErrCrossNamespaceForbidden reports a sourceRef targeting another
	// namespace while --no-cross-namespace-refs is set.
	ErrCrossNamespaceForbidden = errors.New("cross-namespace source reference rejected")
)

// ResolvedArtifact is a ready ExternalArtifact's identity plus the coordinates
// needed to fetch and pin it.
type ResolvedArtifact struct {
	// Namespace and Name identify the resolved ExternalArtifact (always an
	// ExternalArtifact, even when the reference named a producer).
	Namespace string
	Name      string

	// URL is status.artifact.url — the bare HTTP(S) tarball location.
	URL string
	// Revision is status.artifact.revision (an opaque, digest-bearing string).
	Revision string
	// Digest is status.artifact.digest in "<algo>:<hex>" form.
	Digest string
}

// Key is the "namespace/name" key recorded in
// status.lastAttemptedRevisions / status.lastAppliedRevisions.
func (a ResolvedArtifact) Key() string { return a.Namespace + "/" + a.Name }

// Resolver turns a stage SourceReference into a ready ExternalArtifact.
type Resolver struct {
	// NoCrossNamespace rejects a sourceRef whose namespace differs from the
	// owning StageSet's namespace.
	NoCrossNamespace bool
}

// Resolve resolves ref (relative to the owning StageSet's namespace) to a
// ready ExternalArtifact. A ref with no Kind, or Kind=ExternalArtifact, is a
// direct lookup; any other Kind is a producer whose published artifact is
// found through the RFC-0012 spec.sourceRef back-pointer. The returned
// artifact is always Ready with a usable status.artifact.
func (r *Resolver) Resolve(ctx context.Context, c client.Client, ref stagesv1.SourceReference, ownerNS string) (ResolvedArtifact, error) {
	ns := ref.Namespace
	if ns == "" {
		ns = ownerNS
	}
	if ns != ownerNS && r.NoCrossNamespace {
		return ResolvedArtifact{}, fmt.Errorf("%w: %s/%s", ErrCrossNamespaceForbidden, ns, ref.Name)
	}

	kind := ref.Kind
	if kind == "" {
		kind = externalArtifactKind
	}

	var (
		ea  *unstructured.Unstructured
		err error
	)
	if kind == externalArtifactKind {
		ea, err = getExternalArtifact(ctx, c, ns, ref.Name)
	} else {
		ea, err = resolveProducer(ctx, c, ref, ns)
	}
	if err != nil {
		return ResolvedArtifact{}, err
	}

	if ok, why := readyState(ea); !ok {
		return ResolvedArtifact{}, fmt.Errorf("ExternalArtifact %s/%s (%s): %w", ea.GetNamespace(), ea.GetName(), why, ErrSourceNotReady)
	}

	art, err := readArtifact(ea)
	if err != nil {
		return ResolvedArtifact{}, err
	}
	art.Namespace = ea.GetNamespace()
	art.Name = ea.GetName()
	return art, nil
}

func getExternalArtifact(ctx context.Context, c client.Client, ns, name string) (*unstructured.Unstructured, error) {
	ea := newExternalArtifact()
	key := types.NamespacedName{Namespace: ns, Name: name}
	if err := c.Get(ctx, key, ea); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return nil, fmt.Errorf("%w: ExternalArtifact %s/%s", ErrArtifactNotFound, ns, name)
		}
		return nil, fmt.Errorf("get ExternalArtifact %s/%s: %w", ns, name, err)
	}
	return ea, nil
}

// resolveProducer finds the single ExternalArtifact in ns whose
// spec.sourceRef back-pointer names the producer (matched on group, kind, and
// name — version is ignored so a producer can bump its API version without
// breaking consumers).
func resolveProducer(ctx context.Context, c client.Client, ref stagesv1.SourceReference, ns string) (*unstructured.Unstructured, error) {
	wantGroup := groupOf(ref.APIVersion)

	list := newExternalArtifactList()
	if err := c.List(ctx, list, client.InNamespace(ns)); err != nil {
		return nil, fmt.Errorf("list ExternalArtifacts in %s: %w", ns, err)
	}

	var matches []*unstructured.Unstructured
	for i := range list.Items {
		item := &list.Items[i]
		sr, found, err := unstructured.NestedStringMap(item.Object, "spec", "sourceRef")
		if err != nil || !found {
			continue
		}
		if sr["kind"] == ref.Kind && sr["name"] == ref.Name && groupOf(sr["apiVersion"]) == wantGroup {
			matches = append(matches, item)
		}
	}

	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("%w: no ExternalArtifact in %s back-references %s %q (%s)", ErrArtifactNotFound, ns, ref.Kind, ref.Name, ref.APIVersion)
	case 1:
		return matches[0], nil
	default:
		return nil, fmt.Errorf("%w: %d ExternalArtifacts in %s back-reference %s %q", ErrAmbiguousProducer, len(matches), ns, ref.Kind, ref.Name)
	}
}

// readyState reports whether obj carries status.conditions[type=Ready]=True,
// and a short reason string when it does not.
func readyState(obj *unstructured.Unstructured) (bool, string) {
	conds, found, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if err != nil || !found {
		return false, "status.conditions not populated"
	}
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := m["type"].(string); t != "Ready" {
			continue
		}
		s, _ := m["status"].(string)
		if s == "True" {
			return true, ""
		}
		reason, _ := m["reason"].(string)
		return false, fmt.Sprintf("Ready=%s reason=%s", s, reason)
	}
	return false, "no Ready condition"
}

// readArtifact extracts url/revision/digest from status.artifact.
func readArtifact(obj *unstructured.Unstructured) (ResolvedArtifact, error) {
	m, found, err := unstructured.NestedMap(obj.Object, "status", "artifact")
	if err != nil {
		return ResolvedArtifact{}, fmt.Errorf("read status.artifact: %w", err)
	}
	if !found {
		return ResolvedArtifact{}, ErrArtifactMissing
	}
	url, _ := m["url"].(string)
	if url == "" {
		return ResolvedArtifact{}, fmt.Errorf("%w: status.artifact.url is empty", ErrArtifactMissing)
	}
	digest, _ := m["digest"].(string)
	if digest == "" {
		return ResolvedArtifact{}, fmt.Errorf("%w: status.artifact.digest is empty", ErrArtifactMissing)
	}
	rev, _ := m["revision"].(string)
	return ResolvedArtifact{URL: url, Revision: rev, Digest: digest}, nil
}

// groupOf returns the group of an apiVersion ("group/version" -> "group";
// a bare "v1" core version -> "").
func groupOf(apiVersion string) string {
	if i := strings.IndexByte(apiVersion, '/'); i >= 0 {
		return apiVersion[:i]
	}
	return ""
}

// ExternalArtifactGVK is the source-controller ExternalArtifact GVK that
// consumers register (as Unstructured) and watch.
var ExternalArtifactGVK = schema.GroupVersionKind{Group: externalArtifactGroup, Version: externalArtifactVersion, Kind: externalArtifactKind}

func externalArtifactGVK() schema.GroupVersionKind { return ExternalArtifactGVK }

func newExternalArtifact() *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(externalArtifactGVK())
	return u
}

func newExternalArtifactList() *unstructured.UnstructuredList {
	l := &unstructured.UnstructuredList{}
	gvk := externalArtifactGVK()
	gvk.Kind += "List"
	l.SetGroupVersionKind(gvk)
	return l
}
