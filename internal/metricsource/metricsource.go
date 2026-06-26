// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

// Package metricsource resolves a v1.MetricSource to a single scalar and
// evaluates a v1.Threshold against it. It is the shared core both gate families
// consume — the rollout-wide error-budget freeze and the per-stage promotion
// analysis — so neither carries any SLO logic of its own: each reads one number
// and compares it to a bound.
package metricsource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/fluxcd/pkg/apis/meta"
	"k8s.io/client-go/util/jsonpath"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

// ErrSourceUnavailable wraps any failure to obtain a usable scalar: the source
// was unreachable, returned a non-2xx, returned an error status, or its result
// could not be reduced to a single number. It is the signal the gates route to
// their onSourceError policy (fail-open for the freeze, hold for promotion).
var ErrSourceUnavailable = errors.New("metric source unavailable")

// ErrNoSource means the MetricSource set no provider — a spec error the webhook
// normally rejects; surfaced here for the reconciler fallback.
var ErrNoSource = errors.New("metric source has no provider")

// SecretReader returns the data of a Secret in a namespace. The querier uses it
// to read a Prometheus bearer token; a nil reader means no secret lookups.
type SecretReader func(ctx context.Context, namespace, name string) (map[string][]byte, error)

// Querier resolves a MetricSource to a scalar. The reconciler holds one; tests
// substitute a fake so the gate logic is exercised without a live Prometheus.
type Querier interface {
	Query(ctx context.Context, namespace string, src stagesv1.MetricSource) (float64, error)
}

// HTTPQuerier resolves a MetricSource (Prometheus instant query or a JSON
// webhook) to a scalar over HTTP. Its HTTP client pins every dialed address
// through IPValidator, closing the DNS-rebinding window so a metric address
// can't be used to reach loopback/link-local/metadata services.
type HTTPQuerier struct {
	// HTTPClient is used for the query; nil builds a 30s client whose dialer
	// rejects forbidden addresses.
	HTTPClient *http.Client
	// IPValidator pins each resolved address at dial time; nil uses the
	// production loopback/link-local/metadata denylist. Tests inject a permissive
	// validator so httptest loopback listeners stay reachable.
	IPValidator func(net.IP) error
	// Secrets reads the optional bearer-token Secret; nil disables secret reads.
	Secrets SecretReader

	client *http.Client
}

// New builds an HTTPQuerier with a secret reader and optional IP validator.
func New(secrets SecretReader, ipValidator func(net.IP) error) *HTTPQuerier {
	return &HTTPQuerier{Secrets: secrets, IPValidator: ipValidator}
}

// Query resolves src to a single scalar, dispatching on its provider. Every
// failure path wraps ErrSourceUnavailable so callers route it through their
// onSourceError policy rather than treating it as a normal numeric result.
func (q *HTTPQuerier) Query(ctx context.Context, namespace string, src stagesv1.MetricSource) (float64, error) {
	switch {
	case src.Prometheus != nil:
		return q.queryPrometheus(ctx, namespace, src.Prometheus)
	case src.Webhook != nil:
		return q.queryWebhook(ctx, namespace, src.Webhook)
	default:
		return 0, ErrNoSource
	}
}

func (q *HTTPQuerier) queryPrometheus(ctx context.Context, namespace string, p *stagesv1.PrometheusSource) (float64, error) {
	endpoint, err := url.Parse(p.Address)
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") {
		return 0, fmt.Errorf("%w: invalid prometheus address %q", ErrSourceUnavailable, p.Address)
	}
	endpoint.Path += "/api/v1/query"
	qv := url.Values{}
	qv.Set("query", p.Query)
	endpoint.RawQuery = qv.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrSourceUnavailable, err)
	}
	req.Header.Set("Accept", "application/json")
	if err := q.applyBearer(ctx, req, namespace, p.SecretRef); err != nil {
		return 0, err
	}

	body, err := q.fetch(req)
	if err != nil {
		return 0, err
	}
	var pr promResponse
	if err := json.Unmarshal(body, &pr); err != nil {
		return 0, fmt.Errorf("%w: decoding response: %v", ErrSourceUnavailable, err)
	}
	if pr.Status != "success" {
		return 0, fmt.Errorf("%w: prometheus status %q: %s", ErrSourceUnavailable, pr.Status, pr.Error)
	}
	return scalarFromResult(pr.Data)
}

func (q *HTTPQuerier) queryWebhook(ctx context.Context, namespace string, w *stagesv1.WebhookSource) (float64, error) {
	endpoint, err := url.Parse(w.URL)
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") {
		return 0, fmt.Errorf("%w: invalid webhook url %q", ErrSourceUnavailable, w.URL)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrSourceUnavailable, err)
	}
	req.Header.Set("Accept", "application/json")
	if err := q.applyBearer(ctx, req, namespace, w.SecretRef); err != nil {
		return 0, err
	}
	body, err := q.fetch(req)
	if err != nil {
		return 0, err
	}
	var doc any
	if err := json.Unmarshal(body, &doc); err != nil {
		return 0, fmt.Errorf("%w: decoding webhook JSON: %v", ErrSourceUnavailable, err)
	}
	return scalarFromJSONPath(doc, w.JSONPath)
}

// applyBearer reads an optional bearer-token Secret and stamps the
// Authorization header.
func (q *HTTPQuerier) applyBearer(ctx context.Context, req *http.Request, namespace string, ref *meta.LocalObjectReference) error {
	if ref == nil || ref.Name == "" {
		return nil
	}
	if q.Secrets == nil {
		return fmt.Errorf("%w: secretRef set but no secret reader configured", ErrSourceUnavailable)
	}
	data, err := q.Secrets(ctx, namespace, ref.Name)
	if err != nil {
		return fmt.Errorf("%w: reading secret %q: %v", ErrSourceUnavailable, ref.Name, err)
	}
	token, ok := data["token"]
	if !ok || len(token) == 0 {
		return fmt.Errorf("%w: secret %q has no non-empty \"token\" key", ErrSourceUnavailable, ref.Name)
	}
	req.Header.Set("Authorization", "Bearer "+string(token))
	return nil
}

// fetch performs the request and returns the (capped) body. A single-scalar
// response is tiny, so the 1 MiB cap turns a wrong, huge endpoint into an error
// rather than an unbounded read.
func (q *HTTPQuerier) fetch(req *http.Request) ([]byte, error) {
	resp, err := q.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSourceUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%w: source returned HTTP %d", ErrSourceUnavailable, resp.StatusCode)
	}
	body, err := io.ReadAll(http.MaxBytesReader(nil, resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("%w: reading response: %v", ErrSourceUnavailable, err)
	}
	return body, nil
}

func (q *HTTPQuerier) httpClient() *http.Client {
	if q.HTTPClient != nil {
		return q.HTTPClient
	}
	if q.client == nil {
		q.client = &http.Client{
			Timeout:   30 * time.Second,
			Transport: &http.Transport{DialContext: q.safeDialContext},
		}
	}
	return q.client
}

// safeDialContext resolves the host once, refuses the connection if any resolved
// IP is forbidden, then dials a validated address — so a name that passes a
// string check but resolves to a forbidden address never connects.
func (q *HTTPQuerier) safeDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	check := q.ipValidator()
	for _, a := range addrs {
		if check(a.IP) != nil {
			return nil, fmt.Errorf("%w: %s", ErrSourceUnavailable, a.IP)
		}
	}
	var d net.Dialer
	var lastErr error
	for _, a := range addrs {
		conn, derr := d.DialContext(ctx, network, net.JoinHostPort(a.IP.String(), port))
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

func (q *HTTPQuerier) ipValidator() func(net.IP) error {
	if q.IPValidator != nil {
		return q.IPValidator
	}
	return func(ip net.IP) error {
		if ForbiddenIP(ip) {
			return fmt.Errorf("%w: %s", ErrSourceUnavailable, ip)
		}
		return nil
	}
}

// ForbiddenIP denies loopback, link-local (incl. cloud metadata), multicast, and
// unspecified addresses, while allowing in-cluster private ranges — the primary
// metric target is an in-cluster Prometheus on a private address.
func ForbiddenIP(ip net.IP) bool {
	return ip.IsLoopback() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() ||
		ip.IsUnspecified()
}

// PermissiveIP allows any address; for tests reaching loopback listeners.
func PermissiveIP(net.IP) error { return nil }

// promResponse is the subset of the Prometheus query API we read.
type promResponse struct {
	Status string   `json:"status"`
	Error  string   `json:"error"`
	Data   promData `json:"data"`
}

type promData struct {
	ResultType string          `json:"resultType"`
	Result     json.RawMessage `json:"result"`
}

// scalarFromResult reduces a Prometheus result to a single float. A scalar
// result is [ts, "value"]; an instant vector must carry exactly one sample,
// whose value is [ts, "value"]. Anything else (empty/multi-sample vector,
// matrix) can't be a single scalar.
func scalarFromResult(d promData) (float64, error) {
	switch d.ResultType {
	case "scalar":
		var pair [2]json.RawMessage
		if err := json.Unmarshal(d.Result, &pair); err != nil {
			return 0, fmt.Errorf("%w: malformed scalar result: %v", ErrSourceUnavailable, err)
		}
		return sampleValue(pair[1])
	case "vector":
		var samples []struct {
			Value [2]json.RawMessage `json:"value"`
		}
		if err := json.Unmarshal(d.Result, &samples); err != nil {
			return 0, fmt.Errorf("%w: malformed vector result: %v", ErrSourceUnavailable, err)
		}
		if len(samples) != 1 {
			return 0, fmt.Errorf("%w: query returned %d samples, want exactly 1 (the query must reduce to a single scalar)", ErrSourceUnavailable, len(samples))
		}
		return sampleValue(samples[0].Value[1])
	default:
		return 0, fmt.Errorf("%w: unsupported result type %q (the query must return a scalar or single-sample vector)", ErrSourceUnavailable, d.ResultType)
	}
}

// sampleValue parses a Prometheus sample value, which is a JSON string holding a
// float (or one of the special tokens NaN/Inf). NaN is rejected — it is the
// shape a misconfigured query takes (wrong labels → empty → NaN), and treating
// it as a number would silently disable the gate.
func sampleValue(raw json.RawMessage) (float64, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return 0, fmt.Errorf("%w: sample value is not a string: %v", ErrSourceUnavailable, err)
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: unparseable sample value %q: %v", ErrSourceUnavailable, s, err)
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, fmt.Errorf("%w: sample value is %v (a misconfigured query returns NaN/Inf)", ErrSourceUnavailable, v)
	}
	return v, nil
}

// scalarFromJSONPath evaluates a kubectl-style JSONPath against a decoded JSON
// document and reduces the result to a single float. The expression must match
// exactly one numeric (or numeric-string) value.
func scalarFromJSONPath(doc any, expr string) (float64, error) {
	jp := jsonpath.New("metric").AllowMissingKeys(false)
	if err := jp.Parse(expr); err != nil {
		return 0, fmt.Errorf("%w: invalid jsonPath %q: %v", ErrSourceUnavailable, expr, err)
	}
	results, err := jp.FindResults(doc)
	if err != nil {
		return 0, fmt.Errorf("%w: jsonPath %q matched nothing: %v", ErrSourceUnavailable, expr, err)
	}
	var vals []reflect.Value
	for _, group := range results {
		vals = append(vals, group...)
	}
	if len(vals) != 1 {
		return 0, fmt.Errorf("%w: jsonPath %q resolved to %d values, want exactly 1 (it must select a single scalar)", ErrSourceUnavailable, expr, len(vals))
	}
	return toScalar(vals[0].Interface())
}

// toScalar coerces a JSON-decoded value to a float. encoding/json decodes
// numbers as float64 and quoted numbers as strings, so both are accepted; NaN /
// Inf and non-numeric types are rejected (a wrong path that lands on an object
// or a NaN must not silently disable the gate).
func toScalar(v any) (float64, error) {
	switch x := v.(type) {
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return 0, fmt.Errorf("%w: value is %v", ErrSourceUnavailable, x)
		}
		return x, nil
	case json.Number:
		return parseFloatScalar(x.String())
	case string:
		return parseFloatScalar(x)
	default:
		return 0, fmt.Errorf("%w: value %v (%T) is not a number", ErrSourceUnavailable, v, v)
	}
}

func parseFloatScalar(s string) (float64, error) {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, fmt.Errorf("%w: unparseable value %q: %v", ErrSourceUnavailable, s, err)
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, fmt.Errorf("%w: value is %v", ErrSourceUnavailable, f)
	}
	return f, nil
}

// ParseScalar parses a decimal threshold string (freezeThreshold,
// resumeThreshold) into a float.
func ParseScalar(s string) (float64, error) {
	return strconv.ParseFloat(s, 64)
}

// ThresholdSatisfied reports whether value is within th's inclusive min/max
// bounds. An unset bound is no limit on that side. A malformed bound string is
// an error (the webhook normally rejects it; this is the reconciler fallback).
func ThresholdSatisfied(th stagesv1.Threshold, value float64) (bool, error) {
	if th.Min != nil {
		min, err := strconv.ParseFloat(*th.Min, 64)
		if err != nil {
			return false, fmt.Errorf("threshold min %q is not a number: %w", *th.Min, err)
		}
		if value < min {
			return false, nil
		}
	}
	if th.Max != nil {
		max, err := strconv.ParseFloat(*th.Max, 64)
		if err != nil {
			return false, fmt.Errorf("threshold max %q is not a number: %w", *th.Max, err)
		}
		if value > max {
			return false, nil
		}
	}
	return true, nil
}
