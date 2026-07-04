// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

// Package artifact resolves a stage's source reference to a ready Flux artifact
// and fetches the referenced tarball with the source-controller digest and
// size-cap contract. A reference resolves to a source that carries a
// status.artifact — an ExternalArtifact or a classic Flux source
// (GitRepository / OCIRepository / Bucket) consumed directly, or any other kind
// treated as a producer and resolved through its RFC-0012 back-pointer.
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

	// ErrProducerAPIVersionRequired marks a producer-kind sourceRef without an
	// apiVersion: the back-pointer match is on API group, and an empty group
	// can never match a real producer's back-pointer (producers always stamp
	// their group), so the reference is unresolvable by construction.
	ErrProducerAPIVersionRequired = errors.New("producer sourceRef requires apiVersion")

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

	// Verified reflects status.conditions[type=SourceVerified] on the resolved
	// source CR: nil when the source declares no such condition (spec.verify not
	// configured — e.g. an in-cluster ExternalArtifact), else its boolean status.
	// Flux sources with spec.verify (cosign/notation) carry it.
	Verified *bool

	// Pinned reports whether the resolved source is pinned to an immutable
	// revision (OCIRepository spec.ref.digest, GitRepository spec.ref.commit) as
	// opposed to a mutable tag/branch whose content an upstream push can change.
	// nil when pinning does not apply to the resolved kind (an in-cluster
	// ExternalArtifact or a producer back-pointer, whose revision the producer
	// controls).
	Pinned *bool
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

// Resolve resolves ref (relative to the owning StageSet's namespace) to a ready
// artifact. A ref with no Kind, or Kind=ExternalArtifact, and the classic Flux
// sources (GitRepository / OCIRepository / Bucket) are direct lookups — the CR
// carries the status.artifact. Any other Kind is a producer whose published
// artifact is found through the RFC-0012 spec.sourceRef back-pointer. The
// returned artifact is always Ready with a usable status.artifact.
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
	switch {
	case kind == externalArtifactKind:
		ea, err = getDirectSource(ctx, c, ref, ns, externalArtifactKind)
	case isDirectSourceKind(ref):
		// GitRepository / OCIRepository / Bucket expose the same status.artifact
		// + Ready-condition contract as ExternalArtifact, so they are consumed
		// directly rather than through a producer back-pointer.
		ea, err = getDirectSource(ctx, c, ref, ns, kind)
	default:
		ea, err = resolveProducer(ctx, c, ref, ns)
	}
	if err != nil {
		return ResolvedArtifact{}, err
	}

	if ok, why := readyState(ea); !ok {
		return ResolvedArtifact{}, fmt.Errorf("%s %s/%s (%s): %w", ea.GetKind(), ea.GetNamespace(), ea.GetName(), why, ErrSourceNotReady)
	}

	art, err := readArtifact(ea)
	if err != nil {
		return ResolvedArtifact{}, err
	}
	art.Namespace = ea.GetNamespace()
	art.Name = ea.GetName()
	art.Verified = verifiedState(ea)
	art.Pinned = pinnedState(ea)
	return art, nil
}

// pinnedState reports whether the source is pinned to an immutable revision.
// OCIRepository is pinned by spec.ref.digest, GitRepository by spec.ref.commit;
// a Bucket has no immutable ref and is never pinned. Other kinds (ExternalArtifact,
// producer back-pointers) return nil — pinning does not apply, the producer owns
// the revision.
func pinnedState(obj *unstructured.Unstructured) *bool {
	pin := func(fields ...string) *bool {
		v, _, _ := unstructured.NestedString(obj.Object, fields...)
		b := v != ""
		return &b
	}
	switch obj.GetKind() {
	case "OCIRepository":
		return pin("spec", "ref", "digest")
	case "GitRepository":
		return pin("spec", "ref", "commit")
	case "Bucket":
		b := false
		return &b
	default:
		return nil
	}
}

// sourceVerifiedCondition is the condition type Flux source-controller sets on a
// source when its spec.verify (cosign/notation) check passes.
const sourceVerifiedCondition = "SourceVerified"

// verifiedState reports the source's signature-verification state from
// status.conditions[type=SourceVerified]: nil when the source declares no such
// condition (spec.verify not configured), else whether it is True.
func verifiedState(obj *unstructured.Unstructured) *bool {
	conds, found, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if err != nil || !found {
		return nil
	}
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := m["type"].(string); t != sourceVerifiedCondition {
			continue
		}
		s, _ := m["status"].(string)
		v := s == "True"
		return &v
	}
	return nil
}

// directSourceKinds are the classic Flux source kinds (besides ExternalArtifact)
// that publish a status.artifact on the CR itself, so a stage consumes them
// directly rather than through a producer back-pointer.
var directSourceKinds = map[string]bool{
	"GitRepository": true,
	"OCIRepository": true,
	"Bucket":        true,
}

// IsProducerRef reports whether ref resolves through a producer back-pointer —
// any kind other than an ExternalArtifact (or empty kind, its default) or a
// classic Flux source consumed directly. Producer refs need spec.apiVersion:
// the back-pointer match is on API group.
func IsProducerRef(ref stagesv1.SourceReference) bool {
	return ref.Kind != "" && ref.Kind != externalArtifactKind && !isDirectSourceKind(ref)
}

// isDirectSourceKind reports whether ref names a classic Flux source consumed
// directly. The group must be the source-controller group (or unset, which
// defaults to it).
func isDirectSourceKind(ref stagesv1.SourceReference) bool {
	if !directSourceKinds[ref.Kind] {
		return false
	}
	g := groupOf(ref.APIVersion)
	return g == "" || g == externalArtifactGroup
}

// getDirectSource fetches a CR that carries its own status.artifact (an
// ExternalArtifact or a classic Flux source) by name. apiVersion defaults to the
// source-controller group/version.
func getDirectSource(ctx context.Context, c client.Client, ref stagesv1.SourceReference, ns, kind string) (*unstructured.Unstructured, error) {
	apiVersion := ref.APIVersion
	if apiVersion == "" {
		apiVersion = externalArtifactGroup + "/" + externalArtifactVersion
	}
	u := &unstructured.Unstructured{}
	u.SetAPIVersion(apiVersion)
	u.SetKind(kind)
	key := types.NamespacedName{Namespace: ns, Name: ref.Name}
	if err := c.Get(ctx, key, u); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return nil, fmt.Errorf("%w: %s %s/%s", ErrArtifactNotFound, kind, ns, ref.Name)
		}
		return nil, fmt.Errorf("get %s %s/%s: %w", kind, ns, ref.Name, err)
	}
	return u, nil
}

// resolveProducer finds the single ExternalArtifact in ns whose
// spec.sourceRef back-pointer names the producer (matched on group, kind, and
// name — version is ignored so a producer can bump its API version without
// breaking consumers).
func resolveProducer(ctx context.Context, c client.Client, ref stagesv1.SourceReference, ns string) (*unstructured.Unstructured, error) {
	if ref.APIVersion == "" {
		// Without a group the back-pointer comparison below matches nothing:
		// producers stamp a real group into spec.sourceRef.apiVersion, and
		// groupOf("") is the empty group. Say so instead of the misleading
		// "no ExternalArtifact back-references" that a correct back-pointer
		// would otherwise earn.
		return nil, fmt.Errorf("%w: sourceRef kind %q name %q needs apiVersion to identify the producer's API group", ErrProducerAPIVersionRequired, ref.Kind, ref.Name)
	}
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
	if before, _, ok := strings.Cut(apiVersion, "/"); ok {
		return before
	}
	return ""
}

// ExternalArtifactGVK is the source-controller ExternalArtifact GVK that
// consumers register (as Unstructured) and watch.
var ExternalArtifactGVK = schema.GroupVersionKind{Group: externalArtifactGroup, Version: externalArtifactVersion, Kind: externalArtifactKind}

func externalArtifactGVK() schema.GroupVersionKind { return ExternalArtifactGVK }

func newExternalArtifactList() *unstructured.UnstructuredList {
	l := &unstructured.UnstructuredList{}
	gvk := externalArtifactGVK()
	gvk.Kind += "List"
	l.SetGroupVersionKind(gvk)
	return l
}
