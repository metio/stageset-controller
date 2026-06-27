// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

// Package actions executes a stage's typed pre/post/onFailure actions. The
// caller supplies an idempotency ledger (the set of action names already run
// for the pinned snapshot) and a record callback invoked after each successful
// action, so retries and restarts never re-fire a side effect.
//
// Verbs: patch, http, fixed-duration and CEL-expression wait, job, delete, and
// apply. job and apply need a Resolver + Fetcher (and apply needs an Applier)
// wired; without them they fail closed with ErrActionUnsupported so a stage
// fails loudly rather than silently skipping a side effect.
package actions

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/fluxcd/pkg/apis/meta"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	apitypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
	"github.com/metio/stageset-controller/internal/artifact"
	"github.com/metio/stageset-controller/internal/build"
	"github.com/metio/stageset-controller/internal/celeval"
)

// Action sentinels.
var (
	// ErrActionUnsupported reports a verb not implemented in this release
	// (job, CEL-expression wait).
	ErrActionUnsupported = errors.New("action type not yet supported")
	// ErrForbiddenHost reports an http action URL rejected by the SSRF guard.
	ErrForbiddenHost = errors.New("action url host is not allowed")
	// ErrForbiddenAddress reports an http action whose host resolved to a
	// forbidden IP at dial time (the dial-time pin behind the string-level
	// allowedURL check).
	ErrForbiddenAddress = errors.New("action url host resolves to a forbidden address")
	// ErrHTTPClientStatus reports an http action that returned a deterministic
	// 4xx (a malformed/unauthorized request). Retrying the same request gets the
	// same answer, so it is terminal — the retry loop fails fast. The two
	// transient 4xx codes (408 Request Timeout, 429 Too Many Requests) are NOT
	// wrapped in this sentinel, so they keep retrying like 5xx.
	ErrHTTPClientStatus = errors.New("action url returned a client error")
)

const maxResponseBytes = 1 << 16 // bound the action response body we drain

// maxErrorBodyBytes bounds how much of a mismatched response body is folded into
// the error (and thus the StageSet status). Small on purpose: enough to carry an
// upstream error message, not a full page.
const maxErrorBodyBytes = 512

// Executor runs typed actions against the cluster.
type Executor struct {
	// Client is the (impersonated) client actions run under.
	Client client.Client
	// AllowedHosts is the --allowed-action-hosts glob list. Empty means
	// allow-all minus always-denied special-purpose ranges.
	AllowedHosts []string
	// HTTPClient overrides the SSRF-guarded default (tests inject a plain one
	// to reach httptest loopback listeners).
	HTTPClient *http.Client
	// IPValidator pins each resolved address at dial time; a nil value uses the
	// production forbiddenIP check. Tests inject a permissive validator so
	// httptest loopback listeners stay reachable.
	IPValidator func(net.IP) error
	// lookupIP resolves a host to its addresses; nil uses net.DefaultResolver.
	// The seam lets a test point a hostname at loopback/link-local without DNS.
	lookupIP func(ctx context.Context, host string) ([]net.IP, error)
	// Resolver and Fetcher render job and apply actions from an
	// ExternalArtifact; nil makes those actions fail-closed.
	Resolver *artifact.Resolver
	Fetcher  *artifact.Fetcher
	// Applier server-side-applies an apply action's built manifests (and
	// optionally waits for readiness). nil makes apply actions fail-closed.
	// The seam keeps this package free of internal/apply.
	Applier ManifestApplier
}

// ManifestApplier server-side-applies built manifests and, when wait is set,
// blocks until they report Ready. Implemented in the controller package over
// internal/apply so this package stays decoupled from the apply engine.
type ManifestApplier interface {
	Apply(ctx context.Context, objects []*unstructured.Unstructured, wait bool, timeout time.Duration) error
}

// Run executes acts in list order, skipping any whose name is already in done.
// After each success it calls record(name) so the ledger persists before the
// next side effect. The first failure stops execution and is returned.
func (e *Executor) Run(ctx context.Context, namespace string, acts []stagesv1.Action, done map[string]bool, record func(name string) error) error {
	for i := range acts {
		a := &acts[i]
		if done[a.Name] {
			continue
		}
		if err := e.exec(ctx, namespace, a); err != nil {
			return fmt.Errorf("action %q: %w", a.Name, err)
		}
		if record != nil {
			if err := record(a.Name); err != nil {
				return err
			}
		}
	}
	return nil
}

// exec applies the per-action timeout and retry budget around one dispatch.
func (e *Executor) exec(ctx context.Context, ns string, a *stagesv1.Action) error {
	if a.Timeout != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, a.Timeout.Duration)
		defer cancel()
	}
	retries := 0
	if a.Retries != nil && *a.Retries > 0 {
		retries = int(*a.Retries)
	}
	var err error
	for attempt := 0; attempt <= retries; attempt++ {
		if err = e.dispatch(ctx, ns, a); err == nil {
			return nil
		}
		// Config errors will not improve on retry. A host that resolves to a
		// forbidden address is steady-state too — the DNS record, not transient
		// load, is the cause. A deterministic 4xx (malformed/unauthorized
		// request) is terminal as well: the same request returns the same status.
		if errors.Is(err, ErrActionUnsupported) || errors.Is(err, ErrForbiddenHost) || errors.Is(err, ErrForbiddenAddress) || errors.Is(err, ErrHTTPClientStatus) {
			return err
		}
		if attempt == retries {
			break
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(backoff(attempt)):
		}
	}
	return err
}

func (e *Executor) dispatch(ctx context.Context, ns string, a *stagesv1.Action) error {
	switch {
	case a.Patch != nil:
		return e.patch(ctx, ns, a.Patch)
	case a.HTTP != nil:
		return e.httpCall(ctx, ns, a.HTTP)
	case a.Wait != nil:
		return e.wait(ctx, ns, a.Wait)
	case a.Job != nil:
		return e.job(ctx, ns, a.Job)
	case a.Delete != nil:
		return e.deleteObject(ctx, ns, a.Delete)
	case a.Apply != nil:
		return e.applyManifests(ctx, ns, a)
	default:
		return errors.New("action sets no verb")
	}
}

// applyManifests resolves, fetches, and builds the action's artifact, then
// server-side-applies the objects under the run's client (optionally waiting
// for readiness). The objects are not recorded in any stage inventory — apply
// is for transient, rollout-scoped resources torn down by a paired delete.
func (e *Executor) applyManifests(ctx context.Context, ns string, a *stagesv1.Action) error {
	if e.Resolver == nil || e.Fetcher == nil || e.Applier == nil {
		return fmt.Errorf("%w: apply action requires resolver, fetcher, and applier wiring", ErrActionUnsupported)
	}
	ap := a.Apply
	resolved, err := e.Resolver.Resolve(ctx, e.Client, ap.SourceRef, ns)
	if err != nil {
		return fmt.Errorf("resolve apply sourceRef: %w", err)
	}
	files, err := e.Fetcher.Fetch(ctx, resolved.URL, resolved.Digest, "")
	if err != nil {
		return fmt.Errorf("fetch apply artifact: %w", err)
	}
	objects, err := build.Build(files, build.Options{Path: ap.Path}, nil)
	if err != nil {
		return fmt.Errorf("build apply manifests: %w", err)
	}
	timeout := 5 * time.Minute
	if a.Timeout != nil {
		timeout = a.Timeout.Duration
	}
	return e.Applier.Apply(ctx, objects, ap.Wait, timeout)
}

// deleteObject removes the target object under the run's (impersonated) client.
// A missing object is success — delete is idempotent, so a retry or a migration
// that already ran does not fail.
func (e *Executor) deleteObject(ctx context.Context, ns string, d *stagesv1.DeleteAction) error {
	gv, err := schema.ParseGroupVersion(d.Target.APIVersion)
	if err != nil {
		return fmt.Errorf("target apiVersion %q: %w", d.Target.APIVersion, err)
	}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gv.WithKind(d.Target.Kind))
	tns := d.Target.Namespace
	if tns == "" {
		tns = ns
	}
	obj.SetNamespace(tns)
	obj.SetName(d.Target.Name)
	var opts []client.DeleteOption
	switch d.Cascade {
	case "Foreground":
		opts = append(opts, client.PropagationPolicy(metav1.DeletePropagationForeground))
	case "Orphan":
		opts = append(opts, client.PropagationPolicy(metav1.DeletePropagationOrphan))
	}
	// Empty or "Background" keeps the apiserver default (background GC).
	if err := e.Client.Delete(ctx, obj, opts...); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete %s %q: %w", d.Target.Kind, d.Target.Name, err)
	}
	return nil
}

func (e *Executor) patch(ctx context.Context, ns string, p *stagesv1.PatchAction) error {
	gv, err := schema.ParseGroupVersion(p.Target.APIVersion)
	if err != nil {
		return fmt.Errorf("target apiVersion %q: %w", p.Target.APIVersion, err)
	}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gv.WithKind(p.Target.Kind))
	tns := p.Target.Namespace
	if tns == "" {
		tns = ns
	}
	obj.SetNamespace(tns)
	obj.SetName(p.Target.Name)

	patchType := apitypes.StrategicMergePatchType
	if p.Type == "json6902" {
		patchType = apitypes.JSONPatchType
	}
	return e.Client.Patch(ctx, obj, client.RawPatch(patchType, []byte(p.Patch)))
}

func (e *Executor) httpCall(ctx context.Context, ns string, h *stagesv1.HTTPAction) error {
	if err := e.allowedURL(h.URL); err != nil {
		return err
	}
	method := h.Method
	if method == "" {
		method = http.MethodPost
	}
	body := h.Body
	if h.BodyFrom != nil {
		v, err := e.secretValue(ctx, ns, h.BodyFrom)
		if err != nil {
			return err
		}
		body = v
	}
	// #nosec G107 -- the URL is SSRF-validated by allowedURL above (scheme,
	// allowlist, always-denied ranges) and re-validated on every redirect.
	req, err := http.NewRequestWithContext(ctx, method, h.URL, strings.NewReader(body))
	if err != nil {
		return err
	}
	for i := range h.HeadersFrom {
		ref := &h.HeadersFrom[i]
		v, verr := e.secretValue(ctx, ns, ref)
		if verr != nil {
			return verr
		}
		req.Header.Set(ref.Key, v)
	}

	hasSecrets := h.BodyFrom != nil || len(h.HeadersFrom) > 0
	resp, err := e.httpClient(req.URL.Host, hasSecrets).Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if statusAccepted(resp.StatusCode, h.ExpectedStatus) {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
		return nil
	}
	// On a mismatch surface the host and a bounded snippet of the body so the
	// status condition (RBAC-gated) carries enough to diagnose. The snippet is
	// capped at maxErrorBodyBytes; secrets are not echoed back into the request
	// body field, so a response body cannot leak request secrets here.
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	err = fmt.Errorf("%w: %s returned status %d: %s",
		statusClass(resp.StatusCode), req.URL.Host, resp.StatusCode, bodySnippet(snippet))
	return err
}

// statusClass classifies a non-accepted response status for the retry loop. A
// deterministic 4xx (except 408/429) wraps ErrHTTPClientStatus so the retry loop
// fails fast; everything else (5xx, plus the transient 408/429) returns a bare
// error so it keeps retrying.
func statusClass(code int) error {
	if code >= 400 && code < 500 && code != http.StatusRequestTimeout && code != http.StatusTooManyRequests {
		return ErrHTTPClientStatus
	}
	return errUnexpectedStatus
}

// errUnexpectedStatus is the retryable status error (5xx and transient 4xx). It
// is deliberately distinct from ErrHTTPClientStatus so the retry loop's
// errors.Is check only fast-fails on the terminal client-error class.
var errUnexpectedStatus = errors.New("unexpected response status")

// bodySnippet renders a captured response body for an error message: trimmed,
// and "(empty)" when there is nothing to show.
func bodySnippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "(empty)"
	}
	return s
}

func (e *Executor) wait(ctx context.Context, ns string, w *stagesv1.WaitAction) error {
	if w.Duration != nil {
		select {
		case <-time.After(w.Duration.Duration):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if w.Expr != "" {
		if w.Target == nil {
			return errors.New("wait with an expr requires a target")
		}
		return e.pollExpr(ctx, ns, w.Target, w.Expr, w.Timeout)
	}
	return nil
}

// pollExpr polls the target object until the CEL expression holds or the
// timeout (default 5m) elapses. Evaluation errors (e.g. a not-yet-populated
// status) are treated as "not satisfied yet".
func (e *Executor) pollExpr(ctx context.Context, ns string, target *meta.NamespacedObjectKindReference, expr string, timeout *metav1.Duration) error {
	prog, err := celeval.Compile(expr)
	if err != nil {
		return err
	}
	gv, err := schema.ParseGroupVersion(target.APIVersion)
	if err != nil {
		return fmt.Errorf("target apiVersion %q: %w", target.APIVersion, err)
	}
	gvk := gv.WithKind(target.Kind)
	tns := target.Namespace
	if tns == "" {
		tns = ns
	}

	limit := 5 * time.Minute
	if timeout != nil {
		limit = timeout.Duration
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	// Retain the last per-poll Get / CEL-eval failure so a malformed expression
	// or an RBAC-denied Get is surfaced in the timeout message instead of being
	// hidden behind a generic "timed out". A poll that succeeds clears it.
	var lastErr error
	for {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(gvk)
		if gerr := e.Client.Get(ctx, apitypes.NamespacedName{Namespace: tns, Name: target.Name}, obj); gerr != nil {
			lastErr = fmt.Errorf("get target: %w", gerr)
		} else if ok, eerr := prog.EvalBool(obj.Object); eerr != nil {
			lastErr = fmt.Errorf("evaluate expression: %w", eerr)
		} else if ok {
			return nil
		} else {
			lastErr = nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for %q on %s/%s did not complete: %w",
				expr, tns, target.Name, foldLastErr(ctx.Err(), lastErr))
		case <-ticker.C:
		}
	}
}

// foldLastErr combines the timeout cause with the last non-nil per-poll error so
// the message names the real obstacle (bad CEL expr, denied Get) rather than a
// bare deadline. When no poll error was seen, the timeout stands alone.
func foldLastErr(timeoutErr, lastErr error) error {
	if lastErr == nil {
		return timeoutErr
	}
	return fmt.Errorf("%w (last error: %v)", timeoutErr, lastErr)
}

// job renders Job objects from an ExternalArtifact, applies them with a
// run-scoped name suffix derived from the artifact revision, awaits completion,
// then garbage-collects them.
func (e *Executor) job(ctx context.Context, ns string, j *stagesv1.JobAction) error {
	if e.Resolver == nil || e.Fetcher == nil {
		return fmt.Errorf("%w: job actions require a resolver and fetcher", ErrActionUnsupported)
	}
	resolved, err := e.Resolver.Resolve(ctx, e.Client, j.SourceRef, ns)
	if err != nil {
		return fmt.Errorf("resolve job artifact: %w", err)
	}
	files, err := e.Fetcher.Fetch(ctx, resolved.URL, resolved.Digest, "")
	if err != nil {
		return fmt.Errorf("fetch job artifact: %w", err)
	}
	objects, err := build.Build(files, build.Options{Path: j.Path}, nil)
	if err != nil {
		return fmt.Errorf("build job manifests: %w", err)
	}

	// Bound an unbounded action context so a hung job cannot block forever.
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()
	}

	suffix := "-" + shortHash(resolved.Revision)
	created := make([]*unstructured.Unstructured, 0, len(objects))
	for _, o := range objects {
		o.SetName(suffixName(o.GetName(), suffix))
		if o.GetNamespace() == "" {
			o.SetNamespace(ns)
		}
		if cerr := e.Client.Create(ctx, o); cerr != nil && !apierrors.IsAlreadyExists(cerr) {
			cleanupObjects(e.Client, created)
			return fmt.Errorf("create job %s: %w", o.GetName(), cerr)
		}
		created = append(created, o)
	}
	defer cleanupObjects(e.Client, created)

	for _, o := range created {
		if werr := e.awaitJob(ctx, o); werr != nil {
			return werr
		}
	}
	return nil
}

func (e *Executor) awaitJob(ctx context.Context, job *unstructured.Unstructured) error {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	// Retain the last poll Get failure so an RBAC-denied or otherwise failing
	// status read surfaces in the timeout message rather than a bare deadline.
	var lastErr error
	for {
		fresh := &unstructured.Unstructured{}
		fresh.SetGroupVersionKind(job.GroupVersionKind())
		if gerr := e.Client.Get(ctx, apitypes.NamespacedName{Namespace: job.GetNamespace(), Name: job.GetName()}, fresh); gerr != nil {
			lastErr = fmt.Errorf("get job: %w", gerr)
		} else {
			lastErr = nil
			conds, _, _ := unstructured.NestedSlice(fresh.Object, "status", "conditions")
			for _, c := range conds {
				m, ok := c.(map[string]any)
				if !ok {
					continue
				}
				t, _ := m["type"].(string)
				s, _ := m["status"].(string)
				if t == "Complete" && s == "True" {
					return nil
				}
				if t == "Failed" && s == "True" {
					return fmt.Errorf("job %s failed", job.GetName())
				}
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("job %s did not complete: %w", job.GetName(), foldLastErr(ctx.Err(), lastErr))
		case <-ticker.C:
		}
	}
}

func cleanupObjects(c client.Client, objs []*unstructured.Unstructured) {
	for _, o := range objs {
		_ = c.Delete(context.Background(), o, client.PropagationPolicy(metav1.DeletePropagationBackground))
	}
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:8]
}

// suffixName appends suffix to name, truncating the base so the result stays
// within the 63-char DNS-1123 label limit.
func suffixName(name, suffix string) string {
	max := 63 - len(suffix)
	if len(name) > max {
		name = name[:max]
	}
	return name + suffix
}

func (e *Executor) secretValue(ctx context.Context, ns string, ref *meta.SecretKeyReference) (string, error) {
	var sec corev1.Secret
	if err := e.Client.Get(ctx, apitypes.NamespacedName{Namespace: ns, Name: ref.Name}, &sec); err != nil {
		return "", fmt.Errorf("read Secret %q: %w", ref.Name, err)
	}
	v, ok := sec.Data[ref.Key]
	if !ok {
		return "", fmt.Errorf("secret %q has no key %q", ref.Name, ref.Key)
	}
	return string(v), nil
}

// allowedURL enforces the SSRF policy: http(s) only, host in the allowlist
// when one is configured, otherwise allow-all minus loopback/link-local/
// multicast/unspecified literals and "localhost".
func (e *Executor) allowedURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrForbiddenHost, err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return fmt.Errorf("%w: scheme %q", ErrForbiddenHost, u.Scheme)
	}
	host := strings.TrimSuffix(u.Hostname(), ".")
	if host == "" {
		return fmt.Errorf("%w: missing host", ErrForbiddenHost)
	}
	if len(e.AllowedHosts) > 0 {
		for _, pattern := range e.AllowedHosts {
			if ok, _ := path.Match(pattern, host); ok {
				return nil
			}
		}
		return fmt.Errorf("%w: %s not in --allowed-action-hosts", ErrForbiddenHost, host)
	}
	if strings.EqualFold(host, "localhost") {
		return fmt.Errorf("%w: %s", ErrForbiddenHost, host)
	}
	// Parse inet_aton alt-IPv4 forms (0x7f000001, 2130706433, 127.1) too, not
	// just net.ParseIP — a literal that passes the string check but resolves to a
	// forbidden address is otherwise only caught at dial time. Shares the
	// artifact guard's parser so the two SSRF layers agree.
	if ip := artifact.ParseIPAny(host); ip != nil && forbiddenIP(ip) {
		return fmt.Errorf("%w: %s", ErrForbiddenHost, host)
	}
	return nil
}

func (e *Executor) httpClient(origHost string, hasSecrets bool) *http.Client {
	if e.HTTPClient != nil {
		return e.HTTPClient
	}
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			DialContext: e.safeDialContext,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			// Go forwards explicitly-set request headers (and replays the body on
			// 307/308) to redirect targets — it only strips Authorization/Cookie
			// on a host change, not custom headers like X-Api-Key. When the action
			// carries secret material (HeadersFrom / BodyFrom) and the redirect
			// crosses to a different host, refuse to follow it: a compromised or
			// malicious endpoint could otherwise 30x us to an attacker host and
			// harvest the tenant's secret. Same-host redirects (path or
			// http→https) still carry the credentials, which is intended.
			if hasSecrets && req.URL.Host != origHost {
				return fmt.Errorf("refusing cross-host redirect to %q: the action's secret headers/body must not be sent to a different host", req.URL.Host)
			}
			return e.allowedURL(req.URL.String())
		},
	}
}

// safeDialContext resolves the host once, rejects the connection if any
// resolved IP is forbidden, then dials a validated address — closing the
// DNS-rebinding window between check and connect. The string-level allowedURL
// guard runs first, but a hostname (or inet_aton-form literal) that passes it
// can still resolve to a loopback/link-local/metadata address; this is the
// only check that sees the actual dialed IP.
func (e *Executor) safeDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ips, err := e.resolve(ctx, host)
	if err != nil {
		return nil, err
	}
	check := e.ipValidator()
	for _, ip := range ips {
		if check(ip) != nil {
			return nil, fmt.Errorf("%w: %s", ErrForbiddenAddress, ip)
		}
	}
	var d net.Dialer
	var lastErr error
	for _, ip := range ips {
		conn, derr := d.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if derr == nil {
			return conn, nil
		}
		lastErr = derr
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no addresses for %s", host)
	}
	return nil, lastErr
}

func (e *Executor) resolve(ctx context.Context, host string) ([]net.IP, error) {
	if e.lookupIP != nil {
		return e.lookupIP(ctx, host)
	}
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	ips := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		ips = append(ips, a.IP)
	}
	return ips, nil
}

func (e *Executor) ipValidator() func(net.IP) error {
	if e.IPValidator != nil {
		return e.IPValidator
	}
	return func(ip net.IP) error {
		if forbiddenIP(ip) {
			return fmt.Errorf("%w: %s", ErrForbiddenAddress, ip)
		}
		return nil
	}
}

// PermissiveIP allows any resolved address; for tests reaching loopback
// listeners through the dial-time guard.
func PermissiveIP(net.IP) error { return nil }

func forbiddenIP(ip net.IP) bool {
	return ip.IsLoopback() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() ||
		ip.IsUnspecified()
}

func statusAccepted(code int, expected []int32) bool {
	if len(expected) == 0 {
		return code >= 200 && code < 300
	}
	for _, e := range expected {
		if int(e) == code {
			return true
		}
	}
	return false
}

func backoff(attempt int) time.Duration {
	d := time.Duration(attempt+1) * 500 * time.Millisecond
	if d > 5*time.Second {
		return 5 * time.Second
	}
	return d
}
