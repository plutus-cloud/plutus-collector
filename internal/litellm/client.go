// Package litellm is a thin client for a self-hosted LiteLLM proxy's admin spend API.
//
// ─── ⚠ UNVERIFIED AGAINST A LIVE INSTANCE ───────────────────────────────────
//
// Unlike internal/opencost — whose header records being checked against OpenCost's published
// swagger.json, and the real bug that check caught — this client has NOT been validated against
// a running LiteLLM or its OpenAPI document. Every field below is read optimistically (pointers
// and `omitempty`-tolerant decoding, missing values degrading to empty rather than failing the
// parse), and the whole file is deliberately isolated so a correction touches nothing else.
//
// Confirm before flipping the backend's cost source to is_live:
//   - that GET /spend/logs accepts start_date/end_date as YYYY-MM-DD and paginates as assumed;
//   - the JSON spelling of the provider field (assumed `custom_llm_provider`);
//   - where a virtual key's human-readable alias lives (several candidates are tried below);
//   - whether `team_id` or a team alias is the more useful team identity.
//
// ─── WHY /spend/logs AND NOT /user/daily/activity ───────────────────────────
//
// LiteLLM also exposes pre-aggregated daily endpoints, and reading one of those would be less
// work and less data. They were rejected for a specific reason worth recording, because it is
// not obvious: their breakdowns are **per dimension, side by side** — spend by model, spend by
// provider, spend by key — and not the cross-product.
//
// Plutus's ingest contract wants a TUPLE per row (model × provider × key × team), because the
// backend writes one row into four grains from it and every grain must sum to the same total.
// Per-dimension breakdowns cannot produce that: pushing all of them would multiply the day's
// spend by the number of dimensions, and pushing one would leave the other three grains empty.
//
// So the collector reads request-level logs and aggregates them into tuples locally. That is the
// same division of labour the OpenCost agent has — the customer's own network does the reduction
// and only a daily aggregate crosses the wire. Per-request data never leaves their network,
// which is the property that made a gateway connector viable at all rather than a tracing
// product.
package litellm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// MaxPages bounds the pagination loop. A runaway guard, not a tuning knob: a busy gateway can
// legitimately produce a lot of request logs in a day, but an unbounded loop against a
// misbehaving or misconfigured endpoint would spin forever inside one tick and never push
// anything at all — the failure mode being avoided is silence, not slowness.
const MaxPages = 200

// PageSize is requested per page. LiteLLM may cap or ignore it; the loop terminates on a short
// or empty page either way.
const PageSize = 1000

type Client struct {
	BaseURL    string
	MasterKey  string
	HTTPClient *http.Client
}

func New(baseURL, masterKey string, httpClient *http.Client) *Client {
	return &Client{BaseURL: baseURL, MasterKey: masterKey, HTTPClient: httpClient}
}

// LogEntry is the subset of a LiteLLM spend-log record this collector reads.
//
// Every field is optional in the decoder's eyes. A LiteLLM version that omits one produces a row
// with that dimension empty — which the backend already handles by bucketing it under a named
// "Unattributed" sentinel — rather than a failed parse that would lose the whole day.
type LogEntry struct {
	Spend             float64        `json:"spend"`
	Model             string         `json:"model"`
	CustomLLMProvider string         `json:"custom_llm_provider"`
	APIKey            string         `json:"api_key"`
	TeamID            string         `json:"team_id"`
	StartTime         string         `json:"startTime"`
	PromptTokens      int64          `json:"prompt_tokens"`
	CompletionTokens  int64          `json:"completion_tokens"`
	Metadata          map[string]any `json:"metadata"`
}

// KeyAlias returns the most human-readable identifier available for the virtual key that made
// the request, falling back to the key hash.
//
// A hash is a poor thing to show a customer in a cost breakdown, but it is a stable identity and
// showing it beats attributing the spend to nobody. The candidate metadata keys are tried in
// descending order of readability.
func (e LogEntry) KeyAlias() string {
	for _, k := range []string{"user_api_key_alias", "user_api_key_team_alias", "user_api_key_user_id"} {
		if v, ok := e.Metadata[k].(string); ok && v != "" {
			return v
		}
	}
	return e.APIKey
}

// TeamAlias prefers a readable team name from metadata over the raw team id.
func (e LogEntry) TeamAlias() string {
	if v, ok := e.Metadata["user_api_key_team_alias"].(string); ok && v != "" {
		return v
	}
	return e.TeamID
}

// spendLogsResponse tolerates both a bare array and an object wrapping one, because which of
// the two a given LiteLLM version returns is among the things this client has not been able to
// verify. Guessing one and being wrong would fail every parse; accepting both costs a few lines.
type spendLogsResponse struct {
	Data []LogEntry `json:"data"`
}

// FetchSpendLogs returns every spend-log entry in [start, end), paging until the endpoint runs
// out of rows or MaxPages is reached.
func (c *Client) FetchSpendLogs(ctx context.Context, start, end time.Time) ([]LogEntry, error) {
	var all []LogEntry

	for page := 0; page < MaxPages; page++ {
		entries, err := c.fetchPage(ctx, start, end, page)
		if err != nil {
			return nil, err
		}
		all = append(all, entries...)
		// A short page is the last page. An empty one terminates too, which also covers an
		// endpoint that ignores pagination entirely and returns the same full set every time —
		// without this the loop would otherwise run all MaxPages iterations duplicating it.
		if len(entries) < PageSize {
			break
		}
	}

	return all, nil
}

func (c *Client) fetchPage(ctx context.Context, start, end time.Time, page int) ([]LogEntry, error) {
	u, err := url.Parse(c.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("parsing LiteLLM base URL %q: %w", c.BaseURL, err)
	}
	u.Path = "/spend/logs"
	q := u.Query()
	q.Set("start_date", start.UTC().Format("2006-01-02"))
	q.Set("end_date", end.UTC().Format("2006-01-02"))
	q.Set("page", strconv.Itoa(page+1))
	q.Set("page_size", strconv.Itoa(PageSize))
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("building LiteLLM request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.MasterKey)

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("querying LiteLLM at %s: %w", u.Redacted(), err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("reading LiteLLM response: %w", err)
	}

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		// Called out separately because it is the single most likely misconfiguration and the
		// generic message ("HTTP 401") sends people looking in the wrong place: the key needed
		// here is an ADMIN-scoped key, and an ordinary virtual key authenticates fine for
		// inference while returning 401 on every admin route.
		return nil, fmt.Errorf("LiteLLM rejected the master key with HTTP %d — /spend/logs requires a key with admin scope, not an ordinary virtual key", resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("LiteLLM returned HTTP %d for %s: %s", resp.StatusCode, u.Path, truncate(string(body)))
	}

	// Bare array first, since that is the documented shape; fall back to the wrapped object.
	var entries []LogEntry
	if err := json.Unmarshal(body, &entries); err == nil {
		return entries, nil
	}
	var wrapped spendLogsResponse
	if err := json.Unmarshal(body, &wrapped); err != nil {
		return nil, fmt.Errorf("parsing LiteLLM spend logs as JSON: %w (body: %s)", err, truncate(string(body)))
	}
	return wrapped.Data, nil
}

func truncate(s string) string {
	const max = 500
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
