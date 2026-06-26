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
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

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

// PrometheusQuerier runs instant PromQL queries over HTTP. Its HTTP client pins
// every dialed address through IPValidator, closing the DNS-rebinding window so
// a metric address can't be used to reach loopback/link-local/metadata services.
type PrometheusQuerier struct {
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

// New builds a PrometheusQuerier with a secret reader and optional IP validator.
func New(secrets SecretReader, ipValidator func(net.IP) error) *PrometheusQuerier {
	return &PrometheusQuerier{Secrets: secrets, IPValidator: ipValidator}
}

// Query resolves src to a single scalar. Every failure path wraps
// ErrSourceUnavailable so callers route it through their onSourceError policy
// rather than treating it as a normal numeric result.
func (q *PrometheusQuerier) Query(ctx context.Context, namespace string, src stagesv1.MetricSource) (float64, error) {
	if src.Prometheus == nil {
		return 0, ErrNoSource
	}
	p := src.Prometheus

	endpoint, err := url.Parse(p.Address)
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") {
		return 0, fmt.Errorf("%w: invalid prometheus address %q", ErrSourceUnavailable, p.Address)
	}
	endpoint.Path = endpoint.Path + "/api/v1/query"
	qv := url.Values{}
	qv.Set("query", p.Query)
	endpoint.RawQuery = qv.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrSourceUnavailable, err)
	}
	req.Header.Set("Accept", "application/json")
	if p.SecretRef != nil && p.SecretRef.Name != "" {
		if q.Secrets == nil {
			return 0, fmt.Errorf("%w: secretRef set but no secret reader configured", ErrSourceUnavailable)
		}
		data, serr := q.Secrets(ctx, namespace, p.SecretRef.Name)
		if serr != nil {
			return 0, fmt.Errorf("%w: reading secret %q: %v", ErrSourceUnavailable, p.SecretRef.Name, serr)
		}
		token, ok := data["token"]
		if !ok || len(token) == 0 {
			return 0, fmt.Errorf("%w: secret %q has no non-empty \"token\" key", ErrSourceUnavailable, p.SecretRef.Name)
		}
		req.Header.Set("Authorization", "Bearer "+string(token))
	}

	resp, err := q.httpClient().Do(req)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrSourceUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, fmt.Errorf("%w: prometheus returned HTTP %d", ErrSourceUnavailable, resp.StatusCode)
	}

	// Cap the body: a single-scalar response is tiny, so a multi-MiB body is a
	// wrong endpoint, not a real result. Avoids buffering an arbitrary response.
	var pr promResponse
	dec := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 1<<20))
	if err := dec.Decode(&pr); err != nil {
		return 0, fmt.Errorf("%w: decoding response: %v", ErrSourceUnavailable, err)
	}
	if pr.Status != "success" {
		return 0, fmt.Errorf("%w: prometheus status %q: %s", ErrSourceUnavailable, pr.Status, pr.Error)
	}
	return scalarFromResult(pr.Data)
}

func (q *PrometheusQuerier) httpClient() *http.Client {
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
func (q *PrometheusQuerier) safeDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
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

func (q *PrometheusQuerier) ipValidator() func(net.IP) error {
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
