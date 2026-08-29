// Package litellm is a thin client for a self-hosted LiteLLM proxy's admin spend API.
//
// ─── VERIFIED against a live proxy and its own /openapi.json ────────────────
//
// Checked against a running LiteLLM (ghcr.io/berriai/litellm:main-latest) with a Postgres
// spend store, and against the OpenAPI document that instance serves. Two things that check
// caught, both of which would have shipped silently:
//
//   - **The endpoint is /spend/logs/v2, not /spend/logs.** Plain /spend/logs defaults to
//     `summarize=true` and returns a per-day ROLLUP — an object keyed by hashed api-key with
//     `models`/`spend`/`users`, carrying none of the per-request fields below. It also has no
//     `page`/`page_size` parameters at all, so paging it is silently ignored: a first page under
//     the page size terminates by luck, and a busy proxy would have looped re-reading the same
//     rows. /spend/logs/v2 is the paginated route, and returns a real envelope.
//   - **`model` is provider-qualified** ("anthropic/claude-3-5-sonnet-20240620"), duplicating
//     `custom_llm_provider` and reading badly in a cost breakdown. `model_group` is the alias the
//     customer's own engineers request ("claude-sonnet"), which is the name worth showing.
//
// Everything else the field mapping assumed was correct: `custom_llm_provider`,
// `metadata.user_api_key_alias`, `metadata.user_api_key_team_alias`, `spend`, `prompt_tokens`,
// `completion_tokens` and `team_id` are all present and populated as expected.
//
// Fields are still read optimistically — missing values degrade to empty rather than failing the
// parse — because this is a fast-moving upstream and a schema change should cost a dimension,
// not the day's data.
//
// ─── WHY REQUEST LOGS AND NOT /*/daily/activity ─────────────────────────────
//
// LiteLLM exposes several pre-aggregated daily endpoints (/team/daily/activity,
// /customer/daily/activity, /tag/daily/activity, /global/spend/report), and reading one would be
// less work and less data. They were rejected for a specific reason worth recording, because it
// is not obvious: each aggregates along ONE dimension — spend by team, or by customer, or by tag
// — and not the cross-product.
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
// misbehaving endpoint would spin forever inside one tick and never push anything at all — the
// failure mode being avoided is silence, not slowness.
const MaxPages = 200

// PageSize is requested per page.
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
	Spend float64 `json:"spend"`

	// Model is provider-qualified ("openai/gpt-4o"); ModelGroup is the alias the customer's own
	// callers use ("gpt-4o"). ModelName() prefers the latter — see there.
	Model             string         `json:"model"`
	ModelGroup        string         `json:"model_group"`
	CustomLLMProvider string         `json:"custom_llm_provider"`
	APIKey            string         `json:"api_key"`
	TeamID            string         `json:"team_id"`
	StartTime         string         `json:"startTime"`
	PromptTokens      int64          `json:"prompt_tokens"`
	CompletionTokens  int64          `json:"completion_tokens"`
	Metadata          map[string]any `json:"metadata"`
}

// ModelName is what the model dimension should show.
//
// `model_group` is the name the customer configured and their own callers request; `model` is
// LiteLLM's provider-qualified target ("anthropic/claude-3-5-sonnet-20240620"). The qualified
// form duplicates the provider dimension and is not what anyone asked for, so the group wins
// where present.
func (e LogEntry) ModelName() string {
	if e.ModelGroup != "" {
		return e.ModelGroup
	}
	return e.Model
}

// KeyAlias returns the most human-readable identifier for the virtual key that made the request,
// falling back to the key hash.
//
// A hash is a poor thing to show in a cost breakdown, but it is a stable identity and beats
// attributing the spend to nobody. It falls back straight to the hash and NOT to any other
// metadata field: `user_api_key_team_alias` in particular sits right beside the alias and is the
// wrong answer — labelling a key with its team's name silently collapses every key on that team
// into one row in the identity breakdown.
func (e LogEntry) KeyAlias() string {
	if v, ok := e.Metadata["user_api_key_alias"].(string); ok && v != "" {
		return v
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

// spendLogsResponse is /spend/logs/v2's envelope. TotalPages is what terminates the loop; a
// short page is not a reliable signal on its own, since the page size may be capped server-side.
// `page` is deliberately not decoded: the loop counts its own pages, so parsing a field it never
// reads adds nothing and one more way for a response to fail to unmarshal.
type spendLogsResponse struct {
	Data       []LogEntry `json:"data"`
	TotalPages int        `json:"total_pages"`
}

// FetchSpendLogs returns every spend-log entry in [start, end), paging until the endpoint runs
// out of rows or MaxPages is reached.
func (c *Client) FetchSpendLogs(ctx context.Context, start, end time.Time) ([]LogEntry, error) {
	var all []LogEntry

	for page := 1; page <= MaxPages; page++ {
		resp, err := c.fetchPage(ctx, start, end, page)
		if err != nil {
			return nil, err
		}
		all = append(all, resp.Data...)
		// TotalPages is authoritative. The belt-and-braces empty-page check guards the case where
		// it comes back as 0 or absent, which would otherwise run the loop to MaxPages
		// re-reading the same rows.
		if len(resp.Data) == 0 || page >= resp.TotalPages {
			break
		}
	}

	return all, nil
}

func (c *Client) fetchPage(ctx context.Context, start, end time.Time, page int) (*spendLogsResponse, error) {
	u, err := url.Parse(c.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("parsing LiteLLM base URL %q: %w", c.BaseURL, err)
	}
	u.Path = "/spend/logs/v2"
	q := u.Query()
	q.Set("start_date", start.UTC().Format("2006-01-02"))
	q.Set("end_date", end.UTC().Format("2006-01-02"))
	q.Set("page", strconv.Itoa(page))
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

	var parsed spendLogsResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("parsing LiteLLM spend logs as JSON: %w (body: %s)", err, truncate(string(body)))
	}
	return &parsed, nil
}

func truncate(s string) string {
	const max = 500
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
