// Package bifrost is a thin client for a self-hosted Bifrost gateway's (github.com/maximhq/bifrost,
// Maxim's open-source AI gateway) log API, plus the reduction of its per-request records to the
// daily aggregates Plutus ingests.
//
// ─── UNVERIFIED AGAINST A REAL BIFROST ──────────────────────────────────────
//
// Nothing in this package has been run against a live Bifrost. The route, query parameters and
// field names come from reading Bifrost's source (handlers/logging.go's getLogs for the filters,
// framework/logstore/tables.go for the Log, SearchResult, PaginationOptions and SearchStats json
// tags, core/schemas for BifrostLLMUsage). What that does and does not establish:
//
//   - Read from source: GET /api/logs; comma-separated filters; `status`, `start_time`/`end_time`
//     parsed as RFC3339; `limit`, `offset`, `sort_by`, `order`; response
//     `{logs, pagination{total_count,...}, stats{total_cost,...}, has_logs}`; log fields
//     `timestamp`, `provider`, `model`, `status` ("processing"|"success"|"error"), `cost`,
//     `virtual_key_id/name`, `team_id/name`, `customer_id/name`, `token_usage`
//     (`prompt_tokens`, `completion_tokens`, `total_tokens`).
//   - NOT confirmed: a real response, the management API's authentication on a given deployment
//     (so credentials are configuration), the maximum honoured `limit`, and whether the
//     retention policy has already purged the day being asked about.
//
// So the extraction is defensive (see FieldMap), every deviation that can be detected is reported
// as a COUNT in the log, and the collector never invents a value: a field it cannot read is empty
// and the backend buckets it under "Unattributed".
//
// Never logged, anywhere in this package: credentials, prompts, responses, or any record content.
// Only counts, costs and the request path.
package bifrost

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// MaxPages bounds the pagination loop, a runaway guard like the LiteLLM client's.
const MaxPages = 500

// PageSize is requested per page; Bifrost documents 1000 as the maximum.
const PageSize = 1000

// Auth is how to authenticate to the management API. Bifrost's own API is unauthenticated unless
// the operator enables auth, so all of it is optional. Header+Value is the general form
// (e.g. "Authorization: Bearer ..." or "X-Api-Key: ..."); BasicUser/BasicPass is the other.
type Auth struct {
	Header    string
	Value     string
	BasicUser string
	BasicPass string
}

// Entry is the compact projection of one log record that survives extraction. Nothing else from
// the record is retained.
type Entry struct {
	Timestamp  time.Time // zero when absent or unparseable
	Status     string
	Provider   string
	Model      string
	VirtualKey string
	Team       string
	Cost       float64
	HasCost    bool
	Input      int64
	Output     int64
}

// Stats is the subset of /api/logs `stats` used to reconcile the collector's own sum.
type Stats struct {
	TotalRequests int64   `json:"total_requests"`
	TotalCost     float64 `json:"total_cost"`
}

type Client struct {
	BaseURL    string
	Auth       Auth
	Fields     FieldMap
	HTTPClient *http.Client
}

func New(baseURL string, auth Auth, fields FieldMap, httpClient *http.Client) *Client {
	return &Client{BaseURL: baseURL, Auth: auth, Fields: fields, HTTPClient: httpClient}
}

type logsResponse struct {
	Logs       []rawRecord `json:"logs"`
	Pagination struct {
		TotalCount int64 `json:"total_count"`
	} `json:"pagination"`
	Stats Stats `json:"stats"`
}

func (c *Client) query(start, end time.Time, extra url.Values) url.Values {
	q := url.Values{}
	q.Set("start_time", start.UTC().Format(time.RFC3339))
	q.Set("end_time", end.UTC().Format(time.RFC3339))
	// Failed requests carry no meaningful cost (Plutus contract, rule 5).
	q.Set("status", "success")
	for k, vs := range extra {
		q[k] = vs
	}
	return q
}

// FetchLogs returns every successful log in [start, end), paging by offset until a short page,
// the reported total, or MaxPages. It also returns how many records fell outside the window
// despite the filter (a server that ignored start_time/end_time would otherwise get a different
// day's spend pushed under this day's date), and how many were skipped for a non-success status.
func (c *Client) FetchLogs(ctx context.Context, start, end time.Time) (entries []Entry, outsideWindow, notSuccess int, err error) {
	for page := 0; page < MaxPages; page++ {
		q := c.query(start, end, url.Values{
			"limit":   {strconv.Itoa(PageSize)},
			"offset":  {strconv.Itoa(page * PageSize)},
			"sort_by": {"timestamp"},
			"order":   {"asc"},
		})
		var resp logsResponse
		if err = c.get(ctx, "/api/logs", q, &resp); err != nil {
			return nil, 0, 0, err
		}
		for _, rec := range resp.Logs {
			e := c.extract(rec)
			if e.Status != "" && !strings.EqualFold(e.Status, "success") {
				notSuccess++
				continue
			}
			if !e.Timestamp.IsZero() && (e.Timestamp.Before(start) || !e.Timestamp.Before(end)) {
				outsideWindow++
				continue
			}
			entries = append(entries, e)
		}
		got := len(resp.Logs)
		if got == 0 || got < PageSize {
			break
		}
		if t := resp.Pagination.TotalCount; t > 0 && int64((page+1)*PageSize) >= t {
			break
		}
	}
	return entries, outsideWindow, notSuccess, nil
}

// FetchStats reads the server's own aggregate for the same filter, for reconciliation only.
func (c *Client) FetchStats(ctx context.Context, start, end time.Time) (*Stats, error) {
	var st Stats // /api/logs/stats embeds SearchStats, so its fields are top-level
	if err := c.get(ctx, "/api/logs/stats", c.query(start, end, nil), &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// FetchDropped reads the count of requests Bifrost failed to log. Non-zero means the logs
// undercount.
func (c *Client) FetchDropped(ctx context.Context) (int64, error) {
	var resp struct {
		Dropped int64 `json:"dropped_requests"`
	}
	if err := c.get(ctx, "/api/logs/dropped", nil, &resp); err != nil {
		return 0, err
	}
	return resp.Dropped, nil
}

func (c *Client) extract(rec rawRecord) Entry {
	f := c.Fields
	e := Entry{
		Status:     rec.str(f.Status),
		Provider:   rec.str(f.Provider),
		Model:      rec.str(f.Model),
		VirtualKey: rec.str(f.VirtualKey),
		Team:       rec.str(f.Team),
	}
	if ts := rec.str(f.Timestamp); ts != "" {
		if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			e.Timestamp = t.UTC()
		}
	}
	e.Cost, e.HasCost = rec.num(f.Cost)
	if v, ok := rec.num(f.InputTokens); ok {
		e.Input = int64(v)
	}
	if v, ok := rec.num(f.OutputTokens); ok {
		e.Output = int64(v)
	}
	return e
}

func (c *Client) get(ctx context.Context, path string, q url.Values, out any) error {
	u, err := url.Parse(c.BaseURL)
	if err != nil {
		return fmt.Errorf("parsing Bifrost base URL: %w", err)
	}
	u.Path = strings.TrimRight(u.Path, "/") + path
	if q != nil {
		u.RawQuery = q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return fmt.Errorf("building Bifrost request: %w", err)
	}
	if c.Auth.Header != "" && c.Auth.Value != "" {
		req.Header.Set(c.Auth.Header, c.Auth.Value)
	}
	if c.Auth.BasicUser != "" {
		req.SetBasicAuth(c.Auth.BasicUser, c.Auth.BasicPass)
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		// url.Error embeds the URL; the base URL carries no credential (auth is in headers).
		return fmt.Errorf("querying Bifrost %s: %w", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 128<<20))
	if err != nil {
		return fmt.Errorf("reading Bifrost response: %w", err)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("Bifrost rejected the request to %s with HTTP %d — its management API requires authentication here; set BIFROST_API_KEY (with BIFROST_API_KEY_HEADER) or BIFROST_BASIC_USER/BIFROST_BASIC_PASSWORD", path, resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		// The body is deliberately NOT echoed: on this API it can contain request content.
		return fmt.Errorf("Bifrost returned HTTP %d for %s", resp.StatusCode, path)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("parsing Bifrost %s response as JSON: %w", path, err)
	}
	return nil
}
