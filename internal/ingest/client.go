package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// Client POSTs a Batch to Plutus's kubernetes-cost ingest endpoint.
type Client struct {
	URL        string
	APIKey     string
	HTTPClient *http.Client
}

func NewClient(url, apiKey string, httpClient *http.Client) *Client {
	return &Client{URL: url, APIKey: apiKey, HTTPClient: httpClient}
}

// RetryableError wraps an error that a caller (internal/pusher) should retry — a network
// failure or a 5xx from the server. A non-retryable error (4xx — bad request, bad/revoked key,
// suspended account) is returned unwrapped, so retrying it would just waste the backoff budget
// on something that can never succeed without operator intervention.
type RetryableError struct {
	Err error
}

func (e *RetryableError) Error() string { return e.Err.Error() }
func (e *RetryableError) Unwrap() error { return e.Err }

// Push sends the batch and returns the parsed Response. Callers should log resp.Rejected /
// resp.Errors even on a 2xx status — a partial-acceptance response is the customer's only
// visibility into which rows the backend couldn't ingest (design doc §1).
func (c *Client) Push(ctx context.Context, batch Batch) (*Response, error) {
	body, err := json.Marshal(batch)
	if err != nil {
		return nil, fmt.Errorf("marshalling batch: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("building push request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.APIKey)

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, &RetryableError{Err: fmt.Errorf("pushing batch to %s: %w", c.URL, err)}
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, &RetryableError{Err: fmt.Errorf("reading push response: %w", err)}
	}

	if resp.StatusCode >= 500 {
		return nil, &RetryableError{Err: fmt.Errorf("Plutus ingest returned HTTP %d: %s", resp.StatusCode, truncate(string(respBody)))}
	}
	if resp.StatusCode >= 400 {
		// Not retryable: a bad/revoked API key, malformed batch, suspended account or
		// spend-limit-enforced account all return 4xx and will not succeed on retry without
		// operator action (rotate the key, fix CURRENCY, contact support).
		return nil, fmt.Errorf("Plutus ingest rejected the push with HTTP %d: %s", resp.StatusCode, truncate(string(respBody)))
	}

	var parsed Response
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("parsing ingest response as JSON: %w (body: %s)", err, truncate(string(respBody)))
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
