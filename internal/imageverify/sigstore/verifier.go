// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

// Package sigstore is the concrete, dependency-heavy image Verifier: it resolves an
// image ref to a digest, fetches the Sigstore bundle(s) cosign attaches as OCI
// referrers, and verifies them against a policy's keyless authorities through
// sigstore-go. It is deliberately split from the pure internal/imageverify core (an
// arch-go rule keeps that core free of sigstore/go-containerregistry) so the gate's
// extraction/matching logic stays cluster- and dependency-free.
package sigstore

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	ggcrremote "github.com/google/go-containerregistry/pkg/v1/remote"
	protobundle "github.com/sigstore/protobuf-specs/gen/pb-go/bundle/v1"
	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/verify"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

// bundleArtifactPrefix is the OCI artifactType prefix cosign stamps on the referrer
// manifests that carry a new-format Sigstore bundle. Matching the prefix (rather than
// one exact version) keeps the fetch working across bundle spec versions.
const bundleArtifactPrefix = "application/vnd.dev.sigstore.bundle"

// maxBundleBytes caps a single fetched bundle blob so a hostile registry can't feed
// an unbounded body into the parser.
const maxBundleBytes = 8 << 20 // 8 MiB

// Verifier verifies container images against ImageVerificationPolicy keyless
// authorities. Construct it with New; it is safe for concurrent use. The trusted
// Sigstore root is loaded once, lazily, on the first Verify — so a controller whose
// policies never fire pays no startup cost and needs no network at boot.
type Verifier struct {
	keychain        authn.Keychain
	transport       http.RoundTripper
	trustedRootPath string
	logger          *slog.Logger

	once     sync.Once
	verifier *verify.Verifier
	initErr  error
}

// Option configures a Verifier.
type Option func(*Verifier)

// WithKeychain sets the registry auth keychain (default authn.DefaultKeychain,
// which honors the pod's mounted pull secrets / ambient cloud credentials).
func WithKeychain(kc authn.Keychain) Option { return func(v *Verifier) { v.keychain = kc } }

// WithTransport sets the HTTP transport used for registry and TUF traffic.
func WithTransport(t http.RoundTripper) Option { return func(v *Verifier) { v.transport = t } }

// WithTrustedRootPath pins verification to an offline trusted-root JSON instead of
// fetching the public Sigstore root over TUF. Empty fetches the public good instance.
func WithTrustedRootPath(path string) Option {
	return func(v *Verifier) { v.trustedRootPath = path }
}

// WithLogger injects a logger; nil falls back to slog.Default().
func WithLogger(l *slog.Logger) Option { return func(v *Verifier) { v.logger = l } }

// New builds a Verifier. It never fails: the trusted root is loaded on first use.
func New(opts ...Option) *Verifier {
	v := &Verifier{keychain: authn.DefaultKeychain, transport: http.DefaultTransport}
	for _, o := range opts {
		o(v)
	}
	if v.logger == nil {
		v.logger = slog.Default()
	}
	return v
}

// Verify resolves ref to a digest, fetches its attached Sigstore bundles, and checks
// them against the policy's keyless authorities. It returns the digest-pinned ref
// (repository@sha256:…) so the caller can rewrite the manifest to exactly what was
// verified. A verification failure — or an unsupported authority/attestation this
// version does not yet enforce — is a non-nil error, so the gate holds the stage
// fail-closed.
func (v *Verifier) Verify(ctx context.Context, ref string, authorities []stagesv1.VerificationAuthority, require []stagesv1.AttestationRequirement) (string, error) {
	if len(require) > 0 {
		return "", errors.New("attestation requirements are not enforced in this version; remove requireAttestations from the policy")
	}

	parsed, err := name.ParseReference(ref)
	if err != nil {
		return "", fmt.Errorf("parse image ref %q: %w", ref, err)
	}
	remoteOpts := []ggcrremote.Option{
		ggcrremote.WithContext(ctx),
		ggcrremote.WithAuthFromKeychain(v.keychain),
		ggcrremote.WithTransport(v.transport),
	}
	desc, err := ggcrremote.Get(parsed, remoteOpts...)
	if err != nil {
		return "", fmt.Errorf("resolve image %q: %w", ref, err)
	}
	digestRef := parsed.Context().Digest(desc.Digest.String())

	digestHex, err := hex.DecodeString(desc.Digest.Hex)
	if err != nil {
		return "", fmt.Errorf("decode image digest: %w", err)
	}

	entities, err := v.fetchBundles(digestRef, remoteOpts)
	if err != nil {
		return "", fmt.Errorf("fetch signatures for %q: %w", ref, err)
	}

	// The keyless verifier (and its trusted-root load) is built lazily, so a policy
	// with only key authorities verifies fully offline.
	if err := verifyImage(v.trustedVerifier, entities, desc.Digest.Algorithm, digestHex, authorities); err != nil {
		return "", err
	}
	return digestRef.String(), nil
}

// trustedVerifier loads the Sigstore trusted root once and builds the keyless
// verifier (transparency-log inclusion + an observed timestamp, the standard
// Fulcio/Rekor posture).
func (v *Verifier) trustedVerifier() (*verify.Verifier, error) {
	v.once.Do(func() {
		var tr root.TrustedMaterial
		if v.trustedRootPath != "" {
			tr, v.initErr = root.NewTrustedRootFromPath(v.trustedRootPath)
		} else {
			tr, v.initErr = root.FetchTrustedRoot()
		}
		if v.initErr != nil {
			v.initErr = fmt.Errorf("load Sigstore trusted root: %w", v.initErr)
			return
		}
		v.verifier, v.initErr = verify.NewVerifier(tr, verify.WithTransparencyLog(1), verify.WithObserverTimestamps(1))
	})
	return v.verifier, v.initErr
}

// fetchBundles pulls every Sigstore bundle attached to the image as an OCI referrer
// and parses each into a SignedEntity. A registry that predates the referrers API
// returns an empty index, which surfaces downstream as "no bundle attached".
func (v *Verifier) fetchBundles(digestRef name.Digest, remoteOpts []ggcrremote.Option) ([]verify.SignedEntity, error) {
	index, err := ggcrremote.Referrers(digestRef, remoteOpts...)
	if err != nil {
		return nil, err
	}
	manifest, err := index.IndexManifest()
	if err != nil {
		return nil, err
	}

	var entities []verify.SignedEntity
	for _, ref := range manifest.Manifests {
		if !strings.HasPrefix(ref.ArtifactType, bundleArtifactPrefix) {
			continue
		}
		referrer := digestRef.Context().Digest(ref.Digest.String())
		img, err := ggcrremote.Image(referrer, remoteOpts...)
		if err != nil {
			v.logger.Warn("skipping unreadable signature referrer", "ref", referrer.String(), "error", err)
			continue
		}
		layers, err := img.Layers()
		if err != nil {
			v.logger.Warn("skipping signature referrer with unreadable layers", "ref", referrer.String(), "error", err)
			continue
		}
		for _, layer := range layers {
			entity, err := parseBundleLayer(layer)
			if err != nil {
				v.logger.Warn("skipping unparseable signature bundle", "ref", referrer.String(), "error", err)
				continue
			}
			entities = append(entities, entity)
		}
	}
	return entities, nil
}

// layerReader is the subset of a go-containerregistry layer the bundle parser needs;
// naming it keeps parseBundleLayer testable without a live registry.
type layerReader interface {
	Uncompressed() (io.ReadCloser, error)
}

func parseBundleLayer(layer layerReader) (verify.SignedEntity, error) {
	rc, err := layer.Uncompressed()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, maxBundleBytes))
	if err != nil {
		return nil, err
	}
	b := &bundle.Bundle{Bundle: new(protobundle.Bundle)}
	if err := b.UnmarshalJSON(data); err != nil {
		return nil, err
	}
	return b, nil
}
