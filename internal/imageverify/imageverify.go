// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

// Package imageverify extracts the container images a stage's rendered objects
// reference, matches them against cluster ImageVerificationPolicies, and verifies
// them through a pluggable Verifier — so a stage never applies an unverified image.
// The verification engine (sigstore-go) lives behind the Verifier interface; this
// package's core — extraction, glob matching, policy selection — is dependency-free
// and pure.
package imageverify

import (
	"context"
	"regexp"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

// Verifier verifies one image against a policy's authorities and required
// attestations. It resolves the ref to a digest (so the caller can pin it) and
// returns that digest; a verification failure is a non-nil error. Implementations
// hold their own registry keychain and trusted root.
type Verifier interface {
	Verify(ctx context.Context, ref string, authorities []stagesv1.VerificationAuthority, require []stagesv1.AttestationRequirement) (digest string, err error)
}

// ExtractImages returns the deduplicated, sorted set of container images referenced
// by the pod specs in objects, across the standard workload kinds. Images inside
// opaque CRD fields are not seen — the gate covers kinds carrying a real PodSpec.
func ExtractImages(objects []*unstructured.Unstructured) []string {
	seen := map[string]bool{}
	var out []string
	for _, o := range objects {
		for _, img := range imagesInObject(o) {
			if img != "" && !seen[img] {
				seen[img] = true
				out = append(out, img)
			}
		}
	}
	sort.Strings(out)
	return out
}

func imagesInObject(o *unstructured.Unstructured) []string {
	base := podSpecPath(o.GetKind())
	if base == nil {
		return nil
	}
	var imgs []string
	for _, field := range []string{"containers", "initContainers", "ephemeralContainers"} {
		path := append(append([]string{}, base...), field)
		list, found, err := unstructured.NestedSlice(o.Object, path...)
		if !found || err != nil {
			continue
		}
		for _, c := range list {
			m, ok := c.(map[string]any)
			if !ok {
				continue
			}
			if img, ok := m["image"].(string); ok {
				imgs = append(imgs, img)
			}
		}
	}
	return imgs
}

// podSpecPath returns the object-relative path to the PodSpec for a workload kind,
// or nil for a kind that carries none.
func podSpecPath(kind string) []string {
	switch kind {
	case "Pod":
		return []string{"spec"}
	case "Deployment", "StatefulSet", "DaemonSet", "ReplicaSet", "Job":
		return []string{"spec", "template", "spec"}
	case "CronJob":
		return []string{"spec", "jobTemplate", "spec", "template", "spec"}
	default:
		return nil
	}
}

// Match reports the policies that govern imageRef — those whose Images glob matches
// the ref (without its tag/digest) — and whether a matching policy explicitly skips
// it. A skipped image is exempt (the caller records the exemption); an image with no
// matching policy is governed by the caller's deny-by-default posture.
func Match(policies []stagesv1.ImageVerificationPolicy, imageRef string) (matched []stagesv1.ImageVerificationPolicy, skipped bool) {
	name := refName(imageRef)
	for i := range policies {
		p := &policies[i]
		if !anyGlob(p.Spec.Images, name) {
			continue
		}
		if anyGlob(p.Spec.Skip, name) {
			skipped = true
			continue
		}
		matched = append(matched, *p)
	}
	return matched, skipped
}

// refName strips the tag and/or digest from an image ref, leaving the repository
// path the globs match against. "reg.io/app:1.2@sha256:ab" -> "reg.io/app".
func refName(ref string) string {
	if at := strings.IndexByte(ref, '@'); at >= 0 {
		ref = ref[:at]
	}
	// A ':' after the last '/' is a tag (a ':' before it is a registry port).
	if slash := strings.LastIndexByte(ref, '/'); slash >= 0 {
		if colon := strings.IndexByte(ref[slash:], ':'); colon >= 0 {
			return ref[:slash+colon]
		}
		return ref
	}
	if colon := strings.IndexByte(ref, ':'); colon >= 0 {
		return ref[:colon]
	}
	return ref
}

func anyGlob(patterns []string, s string) bool {
	for _, p := range patterns {
		if globMatch(p, s) {
			return true
		}
	}
	return false
}

// globMatch matches s against a glob supporting `*` (any run of non-'/' characters)
// and `**` (any run including '/'). Everything else is literal.
func globMatch(pattern, s string) bool {
	re, err := regexp.Compile(globToRegexp(pattern))
	if err != nil {
		return false
	}
	return re.MatchString(s)
}

func globToRegexp(pattern string) string {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		switch c := pattern[i]; c {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				b.WriteString(".*")
				i++
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	return b.String()
}
