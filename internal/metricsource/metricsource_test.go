// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package metricsource

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/fluxcd/pkg/apis/meta"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

func promServer(t *testing.T, body string, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if q := r.URL.Query().Get("query"); q == "" {
			t.Error("missing query param")
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func querierFor(srv *httptest.Server) *HTTPQuerier {
	return &HTTPQuerier{IPValidator: PermissiveIP, HTTPClient: srv.Client()}
}

func srcFor(srv *httptest.Server, query string) stagesv1.MetricSource {
	return stagesv1.MetricSource{Prometheus: &stagesv1.PrometheusSource{Address: srv.URL, Query: query}}
}

func TestQuery_ScalarResult(t *testing.T) {
	srv := promServer(t, `{"status":"success","data":{"resultType":"scalar","result":[1700000000,"0.42"]}}`, 200)
	got, err := querierFor(srv).Query(context.Background(), "ns", "sa", srcFor(srv, "up"))
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got != 0.42 {
		t.Errorf("value = %v, want 0.42", got)
	}
}

func TestQuery_VectorSingleSample(t *testing.T) {
	srv := promServer(t, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1700000000,"0.9"]}]}}`, 200)
	got, err := querierFor(srv).Query(context.Background(), "ns", "sa", srcFor(srv, "budget"))
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got != 0.9 {
		t.Errorf("value = %v, want 0.9", got)
	}
}

func TestQuery_VectorMultiSampleIsUnavailable(t *testing.T) {
	srv := promServer(t, `{"status":"success","data":{"resultType":"vector","result":[{"value":[0,"1"]},{"value":[0,"2"]}]}}`, 200)
	_, err := querierFor(srv).Query(context.Background(), "ns", "sa", srcFor(srv, "x"))
	if !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("err = %v, want ErrSourceUnavailable", err)
	}
}

func TestQuery_EmptyVectorIsUnavailable(t *testing.T) {
	srv := promServer(t, `{"status":"success","data":{"resultType":"vector","result":[]}}`, 200)
	_, err := querierFor(srv).Query(context.Background(), "ns", "sa", srcFor(srv, "x"))
	if !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("err = %v, want ErrSourceUnavailable", err)
	}
}

func TestQuery_ErrorStatusIsUnavailable(t *testing.T) {
	srv := promServer(t, `{"status":"error","error":"bad query"}`, 200)
	_, err := querierFor(srv).Query(context.Background(), "ns", "sa", srcFor(srv, "x"))
	if !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("err = %v, want ErrSourceUnavailable", err)
	}
}

func TestQuery_HTTP500IsUnavailable(t *testing.T) {
	srv := promServer(t, `boom`, 500)
	_, err := querierFor(srv).Query(context.Background(), "ns", "sa", srcFor(srv, "x"))
	if !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("err = %v, want ErrSourceUnavailable", err)
	}
}

func TestQuery_NaNIsRejected(t *testing.T) {
	srv := promServer(t, `{"status":"success","data":{"resultType":"scalar","result":[0,"NaN"]}}`, 200)
	_, err := querierFor(srv).Query(context.Background(), "ns", "sa", srcFor(srv, "x"))
	if !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("err = %v, want ErrSourceUnavailable (NaN must not pass)", err)
	}
}

func TestQuery_BearerTokenSent(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"scalar","result":[0,"1"]}}`))
	}))
	t.Cleanup(srv.Close)
	q := &HTTPQuerier{
		IPValidator: PermissiveIP,
		HTTPClient:  srv.Client(),
		Secrets: func(_ context.Context, ns, sa, name string) (map[string][]byte, error) {
			if ns != "team-a" || name != "prom-auth" {
				t.Errorf("secret lookup = %s/%s", ns, name)
			}
			// The identity must reach the reader: it is what bounds the read to
			// what the StageSet's own ServiceAccount may see.
			if sa != "tenant-sa" {
				t.Errorf("secret lookup serviceAccount = %q, want %q", sa, "tenant-sa")
			}
			return map[string][]byte{"token": []byte("s3cr3t")}, nil
		},
	}
	src := stagesv1.MetricSource{Prometheus: &stagesv1.PrometheusSource{
		Address: srv.URL, Query: "up", SecretRef: &meta.LocalObjectReference{Name: "prom-auth"},
	}}
	if _, err := q.Query(context.Background(), "team-a", "tenant-sa", src); err != nil {
		t.Fatalf("Query: %v", err)
	}
	if gotAuth != "Bearer s3cr3t" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer s3cr3t")
	}
}

func TestQuery_SecretMissingTokenKey(t *testing.T) {
	srv := promServer(t, `{"status":"success","data":{"resultType":"scalar","result":[0,"1"]}}`, 200)
	q := querierFor(srv)
	q.Secrets = func(context.Context, string, string, string) (map[string][]byte, error) {
		return map[string][]byte{"other": []byte("x")}, nil
	}
	src := srcFor(srv, "up")
	src.Prometheus.SecretRef = &meta.LocalObjectReference{Name: "prom-auth"}
	if _, err := q.Query(context.Background(), "ns", "sa", src); !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("err = %v, want ErrSourceUnavailable", err)
	}
}

func TestQuery_NoProvider(t *testing.T) {
	q := &HTTPQuerier{IPValidator: PermissiveIP}
	if _, err := q.Query(context.Background(), "ns", "sa", stagesv1.MetricSource{}); !errors.Is(err, ErrNoSource) {
		t.Fatalf("err = %v, want ErrNoSource", err)
	}
}

func TestQuery_BadAddress(t *testing.T) {
	q := &HTTPQuerier{IPValidator: PermissiveIP}
	src := stagesv1.MetricSource{Prometheus: &stagesv1.PrometheusSource{Address: "://nope", Query: "up"}}
	if _, err := q.Query(context.Background(), "ns", "sa", src); !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("err = %v, want ErrSourceUnavailable", err)
	}
}

func TestThresholdSatisfied(t *testing.T) {
	cases := []struct {
		name  string
		th    stagesv1.Threshold
		value float64
		want  bool
	}{
		{"no bounds", stagesv1.Threshold{}, 5, true},
		{"max ok", stagesv1.Threshold{Max: new("0.01")}, 0.005, true},
		{"max breached", stagesv1.Threshold{Max: new("0.01")}, 0.02, false},
		{"max boundary inclusive", stagesv1.Threshold{Max: new("0.01")}, 0.01, true},
		{"min ok", stagesv1.Threshold{Min: new("0.05")}, 0.1, true},
		{"min breached", stagesv1.Threshold{Min: new("0.05")}, 0.01, false},
		{"both ok", stagesv1.Threshold{Min: new("0"), Max: new("1")}, 0.5, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ThresholdSatisfied(tc.th, tc.value)
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestThresholdSatisfied_BadBound(t *testing.T) {
	if _, err := ThresholdSatisfied(stagesv1.Threshold{Max: new("abc")}, 1); err == nil {
		t.Fatal("want error for unparseable max")
	}
	if _, err := ThresholdSatisfied(stagesv1.Threshold{Min: new("abc")}, 1); err == nil {
		t.Fatal("want error for unparseable min")
	}
}

func TestParseScalar(t *testing.T) {
	v, err := ParseScalar("0.05")
	if err != nil || v != 0.05 {
		t.Fatalf("ParseScalar = %v, %v", v, err)
	}
	if _, err := ParseScalar("nope"); err == nil {
		t.Fatal("want error")
	}
}

func TestForbiddenIP(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{
		{"127.0.0.1", true},
		{"169.254.169.254", true}, // cloud metadata
		{"0.0.0.0", true},
		{"224.0.0.1", true},
		{"10.0.0.5", false}, // in-cluster private — allowed
		{"192.168.1.1", false},
	}
	for _, tc := range cases {
		if got := ForbiddenIP(net.ParseIP(tc.ip)); got != tc.want {
			t.Errorf("ForbiddenIP(%s) = %v, want %v", tc.ip, got, tc.want)
		}
	}
}

func webhookServer(t *testing.T, body string, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func webhookSrc(srv *httptest.Server, jsonPath string) stagesv1.MetricSource {
	return stagesv1.MetricSource{Webhook: &stagesv1.WebhookSource{URL: srv.URL, JSONPath: jsonPath}}
}

func TestQuery_WebhookNumber(t *testing.T) {
	srv := webhookServer(t, `{"objectives":[{"errorBudgetRemaining":0.73}]}`, 200)
	got, err := querierFor(srv).Query(context.Background(), "ns", "sa", webhookSrc(srv, "{.objectives[0].errorBudgetRemaining}"))
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got != 0.73 {
		t.Errorf("value = %v, want 0.73", got)
	}
}

func TestQuery_WebhookNumericString(t *testing.T) {
	srv := webhookServer(t, `{"remaining":"0.5"}`, 200)
	got, err := querierFor(srv).Query(context.Background(), "ns", "sa", webhookSrc(srv, "{.remaining}"))
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got != 0.5 {
		t.Errorf("value = %v, want 0.5", got)
	}
}

func TestQuery_WebhookNoMatchIsUnavailable(t *testing.T) {
	srv := webhookServer(t, `{"other":1}`, 200)
	_, err := querierFor(srv).Query(context.Background(), "ns", "sa", webhookSrc(srv, "{.missing}"))
	if !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("err = %v, want ErrSourceUnavailable", err)
	}
}

func TestQuery_WebhookMultiMatchIsUnavailable(t *testing.T) {
	srv := webhookServer(t, `{"vals":[1,2,3]}`, 200)
	_, err := querierFor(srv).Query(context.Background(), "ns", "sa", webhookSrc(srv, "{.vals[*]}"))
	if !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("err = %v, want ErrSourceUnavailable", err)
	}
}

func TestQuery_WebhookNonNumericIsUnavailable(t *testing.T) {
	srv := webhookServer(t, `{"obj":{"a":1}}`, 200)
	_, err := querierFor(srv).Query(context.Background(), "ns", "sa", webhookSrc(srv, "{.obj}"))
	if !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("err = %v, want ErrSourceUnavailable (an object is not a scalar)", err)
	}
}

func TestQuery_WebhookHTTP500IsUnavailable(t *testing.T) {
	srv := webhookServer(t, `boom`, 500)
	_, err := querierFor(srv).Query(context.Background(), "ns", "sa", webhookSrc(srv, "{.x}"))
	if !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("err = %v, want ErrSourceUnavailable", err)
	}
}

func TestQuery_WebhookBearerTokenSent(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"v":1}`))
	}))
	t.Cleanup(srv.Close)
	q := &HTTPQuerier{
		IPValidator: PermissiveIP,
		HTTPClient:  srv.Client(),
		Secrets: func(context.Context, string, string, string) (map[string][]byte, error) {
			return map[string][]byte{"token": []byte("hook-tok")}, nil
		},
	}
	src := stagesv1.MetricSource{Webhook: &stagesv1.WebhookSource{URL: srv.URL, JSONPath: "{.v}", SecretRef: &meta.LocalObjectReference{Name: "nobl9"}}}
	if _, err := q.Query(context.Background(), "ns", "sa", src); err != nil {
		t.Fatalf("Query: %v", err)
	}
	if gotAuth != "Bearer hook-tok" {
		t.Errorf("Authorization = %q, want Bearer hook-tok", gotAuth)
	}
}

func TestQuery_WebhookBadURL(t *testing.T) {
	q := &HTTPQuerier{IPValidator: PermissiveIP}
	src := stagesv1.MetricSource{Webhook: &stagesv1.WebhookSource{URL: "://nope", JSONPath: "{.v}"}}
	if _, err := q.Query(context.Background(), "ns", "sa", src); !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("err = %v, want ErrSourceUnavailable", err)
	}
}

// AllowedHosts bounds which hosts a metric query may reach, mirroring the http
// action's allowlist. A metric source is an outbound call to a URL the StageSet
// author supplied, carrying a bearer token — the same exposure an action has.
func TestQuery_AllowedHostsBoundsBothProviders(t *testing.T) {
	srv := promServer(t, `{"status":"success","data":{"resultType":"scalar","result":[0,"1"]}}`, 200)
	host, _, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("SplitHostPort: %v", err)
	}

	t.Run("empty list allows any host", func(t *testing.T) {
		q := querierFor(srv)
		if _, err := q.Query(context.Background(), "ns", "sa", srcFor(srv, "up")); err != nil {
			t.Fatalf("Query: %v", err)
		}
	})

	t.Run("listed host is reachable", func(t *testing.T) {
		q := querierFor(srv)
		q.AllowedHosts = []string{host}
		if _, err := q.Query(context.Background(), "ns", "sa", srcFor(srv, "up")); err != nil {
			t.Fatalf("Query: %v", err)
		}
	})

	t.Run("unlisted prometheus host is refused", func(t *testing.T) {
		q := querierFor(srv)
		q.AllowedHosts = []string{"prometheus.monitoring"}
		if _, err := q.Query(context.Background(), "ns", "sa", srcFor(srv, "up")); !errors.Is(err, ErrSourceUnavailable) {
			t.Fatalf("err = %v, want ErrSourceUnavailable", err)
		}
	})

	// The webhook provider is the exfiltration-shaped one: it sends a bearer
	// token to an author-chosen URL, so the allowlist must cover it too.
	t.Run("unlisted webhook host is refused", func(t *testing.T) {
		q := querierFor(srv)
		q.AllowedHosts = []string{"nobl9.example.com"}
		src := stagesv1.MetricSource{Webhook: &stagesv1.WebhookSource{URL: srv.URL, JSONPath: "{.v}"}}
		if _, err := q.Query(context.Background(), "ns", "sa", src); !errors.Is(err, ErrSourceUnavailable) {
			t.Fatalf("err = %v, want ErrSourceUnavailable", err)
		}
	})

	t.Run("glob patterns match", func(t *testing.T) {
		q := querierFor(srv)
		q.AllowedHosts = []string{"*"}
		if _, err := q.Query(context.Background(), "ns", "sa", srcFor(srv, "up")); err != nil {
			t.Fatalf("Query: %v", err)
		}
	})

	// A refused host must be refused before the token is read, so an unlisted
	// endpoint cannot even trigger the Secret lookup.
	t.Run("refused host never reads the secret", func(t *testing.T) {
		read := false
		q := querierFor(srv)
		q.AllowedHosts = []string{"elsewhere.example.com"}
		q.Secrets = func(context.Context, string, string, string) (map[string][]byte, error) {
			read = true
			return map[string][]byte{"token": []byte("s3cr3t")}, nil
		}
		src := stagesv1.MetricSource{Webhook: &stagesv1.WebhookSource{
			URL: srv.URL, JSONPath: "{.v}", SecretRef: &meta.LocalObjectReference{Name: "tok"},
		}}
		if _, err := q.Query(context.Background(), "ns", "sa", src); !errors.Is(err, ErrSourceUnavailable) {
			t.Fatalf("err = %v, want ErrSourceUnavailable", err)
		}
		if read {
			t.Error("secret was read for a host outside the allowlist")
		}
	})
}

// --- redirect policy -------------------------------------------------------
//
// These exercise the DEFAULT client (HTTPClient left nil) because CheckRedirect
// lives on it; a test that injects srv.Client() would not see the guard at all.

// scalarServer answers any path with a fixed scalar result.
func scalarServer(t *testing.T, value string, seenAuth *string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if seenAuth != nil {
			*seenAuth = r.Header.Get("Authorization")
		}
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"scalar","result":[0,"` + value + `"]}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// redirectServer sends every request on to target.
func redirectServer(t *testing.T, target string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target, http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// sameAddrAs renders srv's address under a different host spelling, so a test
// can cross a HOST boundary while still reaching a loopback listener.
func sameAddrAs(t *testing.T, srv *httptest.Server, host string) string {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse %q: %v", srv.URL, err)
	}
	return "http://" + net.JoinHostPort(host, u.Port())
}

// A source may not redirect the query to a host outside the allowlist: the
// allowlist bounds where the query ends up, not merely where it starts.
func TestQuery_RedirectOffAllowlistIsRefused(t *testing.T) {
	off := scalarServer(t, "42", nil)
	entry := redirectServer(t, sameAddrAs(t, off, "localhost")+"/api/v1/query")

	q := New(nil, PermissiveIP, []string{"127.0.0.1"}) // "localhost" is NOT allowed
	_, err := q.Query(context.Background(), "ns", "sa",
		stagesv1.MetricSource{Prometheus: &stagesv1.PrometheusSource{Address: entry.URL, Query: "up"}})
	if !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("err = %v, want ErrSourceUnavailable", err)
	}
	if !strings.Contains(err.Error(), "allowed-action-hosts") {
		t.Fatalf("err = %v, want it to name the allowlist", err)
	}
}

// A redirect that stays inside the allowlist is followed, so the guard bounds
// the destination without breaking a source that legitimately redirects.
func TestQuery_RedirectWithinAllowlistIsFollowed(t *testing.T) {
	target := scalarServer(t, "42", nil)
	entry := redirectServer(t, sameAddrAs(t, target, "localhost")+"/api/v1/query")

	q := New(nil, PermissiveIP, []string{"127.0.0.1", "localhost"})
	got, err := q.Query(context.Background(), "ns", "sa",
		stagesv1.MetricSource{Prometheus: &stagesv1.PrometheusSource{Address: entry.URL, Query: "up"}})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got != 42 {
		t.Fatalf("value = %v, want 42", got)
	}
}

// A query carrying a bearer token refuses to follow a host change even when both
// hosts are allow-listed: Go forwards Authorization to a subdomain of the
// original, so the allowlist alone would not keep the tenant's token off a host
// the operator never meant to hand it to.
func TestQuery_CrossHostRedirectWithTokenIsRefused(t *testing.T) {
	var reachedAuth string
	target := scalarServer(t, "42", &reachedAuth)
	entry := redirectServer(t, sameAddrAs(t, target, "localhost")+"/api/v1/query")

	q := New(
		func(context.Context, string, string, string) (map[string][]byte, error) {
			return map[string][]byte{"token": []byte("s3cr3t")}, nil
		},
		PermissiveIP,
		[]string{"127.0.0.1", "localhost"}, // both allowed; the credential is the bar
	)
	_, err := q.Query(context.Background(), "ns", "sa", stagesv1.MetricSource{
		Prometheus: &stagesv1.PrometheusSource{
			Address: entry.URL, Query: "up", SecretRef: &meta.LocalObjectReference{Name: "tok"},
		},
	})
	if !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("err = %v, want ErrSourceUnavailable", err)
	}
	if reachedAuth != "" {
		t.Fatalf("the redirect target saw Authorization %q; it must never be reached", reachedAuth)
	}
}

// The credential guard is about crossing hosts, not about redirects: a source
// that redirects within its own host still carries the token, which is what
// configuring one is for.
func TestQuery_SameHostRedirectKeepsToken(t *testing.T) {
	var gotAuth string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/query", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/moved", http.StatusFound)
	})
	mux.HandleFunc("/moved", func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"scalar","result":[0,"7"]}}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	q := New(
		func(context.Context, string, string, string) (map[string][]byte, error) {
			return map[string][]byte{"token": []byte("s3cr3t")}, nil
		},
		PermissiveIP, nil,
	)
	got, err := q.Query(context.Background(), "ns", "sa", stagesv1.MetricSource{
		Prometheus: &stagesv1.PrometheusSource{
			Address: srv.URL, Query: "up", SecretRef: &meta.LocalObjectReference{Name: "tok"},
		},
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got != 7 || gotAuth != "Bearer s3cr3t" {
		t.Fatalf("value=%v auth=%q, want 7 and the token forwarded", got, gotAuth)
	}
}

// Declaring CheckRedirect removes Go's own cap, so the chain bound has to be
// ours. A source looping on itself must terminate rather than spin.
func TestQuery_RedirectChainIsBounded(t *testing.T) {
	var hops int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hops++
		http.Redirect(w, r, "/again", http.StatusFound)
	}))
	t.Cleanup(srv.Close)

	q := New(nil, PermissiveIP, nil)
	_, err := q.Query(context.Background(), "ns", "sa",
		stagesv1.MetricSource{Prometheus: &stagesv1.PrometheusSource{Address: srv.URL, Query: "up"}})
	if !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("err = %v, want ErrSourceUnavailable", err)
	}
	if hops > maxRedirects+1 {
		t.Fatalf("served %d hops, want the chain capped near %d", hops, maxRedirects)
	}
}
