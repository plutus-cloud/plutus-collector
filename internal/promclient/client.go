// Package promclient is a minimal client for the Prometheus HTTP query API (instant queries
// only), used by the preflight checks to ask the store behind OpenCost what it actually holds.
//
// It speaks the API that Prometheus, Mimir, Cortex, Thanos Query and Grafana Cloud share, with
// the authentication OpenCost itself supports (basic, bearer, mTLS, X-Scope-OrgID), so the
// agent can be pointed at the same endpoint with the same credentials as OpenCost. It depends
// only on the standard library.
package promclient

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config says where the store is and how to authenticate to it. Credentials are never logged by
// this package and never appear in its error strings.
type Config struct {
	// URL is the base of the query API as OpenCost is given it: for Prometheus the server root,
	// for Grafana Cloud ".../api/prom", for Mimir ".../prometheus". "/api/v1/query" is appended.
	URL string

	Username, Password string
	BearerToken        string
	// TenantID is sent as X-Scope-OrgID (Mimir, Cortex).
	TenantID string

	InsecureSkipVerify bool
	CAFile             string
	CertFile, KeyFile  string

	Timeout time.Duration
}

// Client queries one Prometheus-compatible endpoint.
type Client struct {
	cfg  Config
	http *http.Client
}

// Sample is one element of an instant-query vector result.
type Sample struct {
	Metric map[string]string
	Value  float64
	// Time is the sample's timestamp as reported by the store.
	Time time.Time
}

// HTTPError is a non-2xx response from the store.
type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.Status, e.Body)
}

// New builds a Client, loading any TLS material named in cfg.
func New(cfg Config) (*Client, error) {
	if cfg.Timeout == 0 {
		cfg.Timeout = 20 * time.Second
	}
	// InsecureSkipVerify is opt-in and mirrors OpenCost's own setting of the same name, so the
	// agent can reach the endpoint OpenCost reaches; it is off unless the operator turns it on.
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: cfg.InsecureSkipVerify}
	if cfg.CAFile != "" {
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("reading Prometheus CA file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("Prometheus CA file contains no certificates")
		}
		tlsCfg.RootCAs = pool
	}
	if (cfg.CertFile == "") != (cfg.KeyFile == "") {
		return nil, errors.New("set both the Prometheus client certificate and key, or neither")
	}
	if cfg.CertFile != "" {
		cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("loading Prometheus client certificate: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}
	return &Client{
		cfg: cfg,
		http: &http.Client{
			Timeout:   cfg.Timeout,
			Transport: &http.Transport{TLSClientConfig: tlsCfg, Proxy: http.ProxyFromEnvironment},
		},
	}, nil
}

type queryResponse struct {
	Status    string `json:"status"`
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
	Data      struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Value  []json.RawMessage `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

// Query runs an instant query and returns its vector. A scalar result is returned as a single
// sample with no labels.
func (c *Client) Query(ctx context.Context, expr string) ([]Sample, error) {
	endpoint := strings.TrimRight(c.cfg.URL, "/") + "/api/v1/query"
	form := url.Values{"query": {expr}}

	// POST with a form body: queries with long selectors would otherwise hit URL length limits
	// in front proxies, and every Prometheus-compatible API accepts it.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("building query request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	switch {
	case c.cfg.BearerToken != "":
		req.Header.Set("Authorization", "Bearer "+c.cfg.BearerToken)
	case c.cfg.Username != "" || c.cfg.Password != "":
		req.SetBasicAuth(c.cfg.Username, c.cfg.Password)
	}
	if c.cfg.TenantID != "" {
		req.Header.Set("X-Scope-OrgID", c.cfg.TenantID)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		// url.Error echoes the request URL. Config refuses userinfo in it, but this package can
		// be built without that check, so any password is redacted here as well.
		var ue *url.Error
		if errors.As(err, &ue) {
			if u, perr := url.Parse(ue.URL); perr == nil {
				ue.URL = u.Redacted()
			} else {
				ue.URL = "(unparseable url)"
			}
		}
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("reading query response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &HTTPError{Status: resp.StatusCode, Body: truncate(string(body), 300)}
	}

	var parsed queryResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("the endpoint answered with something that is not a Prometheus query response: %w", err)
	}
	if parsed.Status != "success" {
		return nil, fmt.Errorf("query failed: %s: %s", parsed.ErrorType, parsed.Error)
	}

	out := make([]Sample, 0, len(parsed.Data.Result))
	for _, r := range parsed.Data.Result {
		s, err := parseValue(r.Value)
		if err != nil {
			return nil, err
		}
		s.Metric = r.Metric
		out = append(out, s)
	}
	return out, nil
}

// parseValue reads Prometheus's [<unix seconds>, "<value>"] pair.
func parseValue(raw []json.RawMessage) (Sample, error) {
	if len(raw) != 2 {
		return Sample{}, errors.New("malformed sample in query response")
	}
	var ts float64
	if err := json.Unmarshal(raw[0], &ts); err != nil {
		return Sample{}, fmt.Errorf("malformed sample timestamp: %w", err)
	}
	var vs string
	if err := json.Unmarshal(raw[1], &vs); err != nil {
		return Sample{}, fmt.Errorf("malformed sample value: %w", err)
	}
	v, err := strconv.ParseFloat(vs, 64)
	if err != nil {
		return Sample{}, fmt.Errorf("malformed sample value: %w", err)
	}
	sec := int64(ts)
	return Sample{Value: v, Time: time.Unix(sec, int64((ts-float64(sec))*1e9)).UTC()}, nil
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
