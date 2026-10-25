package promclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestClient(t *testing.T, h http.HandlerFunc, mutate func(*Config)) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	cfg := Config{URL: srv.URL + "/api/prom"}
	if mutate != nil {
		mutate(&cfg)
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestQuery_ParsesAVectorAndAppendsThePath(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/prom/api/v1/query" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_ = r.ParseForm()
		if r.Form.Get("query") != `count(up)` {
			t.Errorf("unexpected query %q", r.Form.Get("query"))
		}
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"cluster":"a"},"value":[1700000000.5,"3"]}]}}`))
	}, nil)

	got, err := c.Query(context.Background(), "count(up)")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Value != 3 || got[0].Metric["cluster"] != "a" || got[0].Time.Unix() != 1700000000 {
		t.Errorf("unexpected result %+v", got)
	}
}

func TestQuery_SendsConfiguredCredentials(t *testing.T) {
	var gotAuth, gotTenant string
	var gotUser, gotPass string
	handler := func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotUser, gotPass, _ = r.BasicAuth()
		gotTenant = r.Header.Get("X-Scope-OrgID")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}

	basic := newTestClient(t, handler, func(c *Config) { c.Username, c.Password, c.TenantID = "u", "p", "tenant-1" })
	if _, err := basic.Query(context.Background(), "up"); err != nil {
		t.Fatal(err)
	}
	if gotUser != "u" || gotPass != "p" || gotTenant != "tenant-1" {
		t.Errorf("basic auth / tenant not sent: %q %q %q", gotUser, gotPass, gotTenant)
	}

	bearer := newTestClient(t, handler, func(c *Config) { c.BearerToken = "tok" })
	if _, err := bearer.Query(context.Background(), "up"); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("bearer token not sent: %q", gotAuth)
	}
}

func TestQuery_ReportsHTTPStatusesAsTypedErrors(t *testing.T) {
	for _, status := range []int{401, 403, 404, 502} {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "nope", status)
		}, nil)
		_, err := c.Query(context.Background(), "up")
		var he *HTTPError
		if !errors.As(err, &he) || he.Status != status {
			t.Errorf("status %d: got %v", status, err)
		}
	}
}

func TestQuery_RejectsANonPrometheusAnswerAndAQueryError(t *testing.T) {
	html := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html>login</html>`))
	}, nil)
	if _, err := html.Query(context.Background(), "up"); err == nil || !strings.Contains(err.Error(), "not a Prometheus query response") {
		t.Errorf("expected a not-prometheus error, got %v", err)
	}

	bad := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"error","errorType":"bad_data","error":"parse error"}`))
	}, nil)
	if _, err := bad.Query(context.Background(), "up"); err == nil || !strings.Contains(err.Error(), "parse error") {
		t.Errorf("expected the query error, got %v", err)
	}
}

func TestNew_RejectsHalfAClientCertificate(t *testing.T) {
	if _, err := New(Config{URL: "https://x", CertFile: "/tmp/c.pem"}); err == nil {
		t.Error("a client certificate without its key must be rejected")
	}
}
