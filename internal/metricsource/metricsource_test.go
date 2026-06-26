// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package metricsource

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/fluxcd/pkg/apis/meta"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

func ptr(s string) *string { return &s }

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

func querierFor(srv *httptest.Server) *PrometheusQuerier {
	return &PrometheusQuerier{IPValidator: PermissiveIP, HTTPClient: srv.Client()}
}

func srcFor(srv *httptest.Server, query string) stagesv1.MetricSource {
	return stagesv1.MetricSource{Prometheus: &stagesv1.PrometheusSource{Address: srv.URL, Query: query}}
}

func TestQuery_ScalarResult(t *testing.T) {
	srv := promServer(t, `{"status":"success","data":{"resultType":"scalar","result":[1700000000,"0.42"]}}`, 200)
	got, err := querierFor(srv).Query(context.Background(), "ns", srcFor(srv, "up"))
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got != 0.42 {
		t.Errorf("value = %v, want 0.42", got)
	}
}

func TestQuery_VectorSingleSample(t *testing.T) {
	srv := promServer(t, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1700000000,"0.9"]}]}}`, 200)
	got, err := querierFor(srv).Query(context.Background(), "ns", srcFor(srv, "budget"))
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got != 0.9 {
		t.Errorf("value = %v, want 0.9", got)
	}
}

func TestQuery_VectorMultiSampleIsUnavailable(t *testing.T) {
	srv := promServer(t, `{"status":"success","data":{"resultType":"vector","result":[{"value":[0,"1"]},{"value":[0,"2"]}]}}`, 200)
	_, err := querierFor(srv).Query(context.Background(), "ns", srcFor(srv, "x"))
	if !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("err = %v, want ErrSourceUnavailable", err)
	}
}

func TestQuery_EmptyVectorIsUnavailable(t *testing.T) {
	srv := promServer(t, `{"status":"success","data":{"resultType":"vector","result":[]}}`, 200)
	_, err := querierFor(srv).Query(context.Background(), "ns", srcFor(srv, "x"))
	if !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("err = %v, want ErrSourceUnavailable", err)
	}
}

func TestQuery_ErrorStatusIsUnavailable(t *testing.T) {
	srv := promServer(t, `{"status":"error","error":"bad query"}`, 200)
	_, err := querierFor(srv).Query(context.Background(), "ns", srcFor(srv, "x"))
	if !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("err = %v, want ErrSourceUnavailable", err)
	}
}

func TestQuery_HTTP500IsUnavailable(t *testing.T) {
	srv := promServer(t, `boom`, 500)
	_, err := querierFor(srv).Query(context.Background(), "ns", srcFor(srv, "x"))
	if !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("err = %v, want ErrSourceUnavailable", err)
	}
}

func TestQuery_NaNIsRejected(t *testing.T) {
	srv := promServer(t, `{"status":"success","data":{"resultType":"scalar","result":[0,"NaN"]}}`, 200)
	_, err := querierFor(srv).Query(context.Background(), "ns", srcFor(srv, "x"))
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
	q := &PrometheusQuerier{
		IPValidator: PermissiveIP,
		HTTPClient:  srv.Client(),
		Secrets: func(_ context.Context, ns, name string) (map[string][]byte, error) {
			if ns != "team-a" || name != "prom-auth" {
				t.Errorf("secret lookup = %s/%s", ns, name)
			}
			return map[string][]byte{"token": []byte("s3cr3t")}, nil
		},
	}
	src := stagesv1.MetricSource{Prometheus: &stagesv1.PrometheusSource{
		Address: srv.URL, Query: "up", SecretRef: &meta.LocalObjectReference{Name: "prom-auth"},
	}}
	if _, err := q.Query(context.Background(), "team-a", src); err != nil {
		t.Fatalf("Query: %v", err)
	}
	if gotAuth != "Bearer s3cr3t" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer s3cr3t")
	}
}

func TestQuery_SecretMissingTokenKey(t *testing.T) {
	srv := promServer(t, `{"status":"success","data":{"resultType":"scalar","result":[0,"1"]}}`, 200)
	q := querierFor(srv)
	q.Secrets = func(context.Context, string, string) (map[string][]byte, error) {
		return map[string][]byte{"other": []byte("x")}, nil
	}
	src := srcFor(srv, "up")
	src.Prometheus.SecretRef = &meta.LocalObjectReference{Name: "prom-auth"}
	if _, err := q.Query(context.Background(), "ns", src); !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("err = %v, want ErrSourceUnavailable", err)
	}
}

func TestQuery_NoProvider(t *testing.T) {
	q := &PrometheusQuerier{IPValidator: PermissiveIP}
	if _, err := q.Query(context.Background(), "ns", stagesv1.MetricSource{}); !errors.Is(err, ErrNoSource) {
		t.Fatalf("err = %v, want ErrNoSource", err)
	}
}

func TestQuery_BadAddress(t *testing.T) {
	q := &PrometheusQuerier{IPValidator: PermissiveIP}
	src := stagesv1.MetricSource{Prometheus: &stagesv1.PrometheusSource{Address: "://nope", Query: "up"}}
	if _, err := q.Query(context.Background(), "ns", src); !errors.Is(err, ErrSourceUnavailable) {
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
		{"max ok", stagesv1.Threshold{Max: ptr("0.01")}, 0.005, true},
		{"max breached", stagesv1.Threshold{Max: ptr("0.01")}, 0.02, false},
		{"max boundary inclusive", stagesv1.Threshold{Max: ptr("0.01")}, 0.01, true},
		{"min ok", stagesv1.Threshold{Min: ptr("0.05")}, 0.1, true},
		{"min breached", stagesv1.Threshold{Min: ptr("0.05")}, 0.01, false},
		{"both ok", stagesv1.Threshold{Min: ptr("0"), Max: ptr("1")}, 0.5, true},
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
	if _, err := ThresholdSatisfied(stagesv1.Threshold{Max: ptr("abc")}, 1); err == nil {
		t.Fatal("want error for unparseable max")
	}
	if _, err := ThresholdSatisfied(stagesv1.Threshold{Min: ptr("abc")}, 1); err == nil {
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
