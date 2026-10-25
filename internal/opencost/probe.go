package opencost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// The calls below are for preflight: they ask OpenCost whether it is computing anything and what
// it believes about its environment, without mapping the answer into cost rows.

func (c *Client) getJSON(ctx context.Context, path string, query url.Values, out any) error {
	u := c.BaseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("building OpenCost request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("querying OpenCost at %s: %w", redactURL(c.BaseURL), redactErr(err))
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return fmt.Errorf("reading OpenCost response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("OpenCost returned HTTP %d: %s", resp.StatusCode, truncate(string(body), 300))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("parsing OpenCost response as JSON: %w", err)
	}
	return nil
}

// redactURL strips any password from a URL before it reaches an error string. Config refuses
// credentials in OPENCOST_URL; this is the second line of defence.
func redactURL(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		return u.Redacted()
	}
	return "(unparseable url)"
}

// redactErr redacts the URL a *url.Error carries, which net/http echoes into its messages.
func redactErr(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		ue.URL = redactURL(ue.URL)
	}
	return err
}

// CountAllocations returns how many priced allocations OpenCost computes for window ("1h",
// "6h", ...), aggregated by namespace. Idle capacity is not counted: it exists whenever nodes
// do, so it says nothing about whether workloads are being attributed.
func (c *Client) CountAllocations(ctx context.Context, window string) (int, error) {
	var parsed struct {
		Data []map[string]struct {
			TotalCost *float64 `json:"totalCost"`
			CPUCost   *float64 `json:"cpuCost"`
			RAMCost   *float64 `json:"ramCost"`
		} `json:"data"`
		Message string `json:"message"`
	}
	q := url.Values{"window": {window}, "aggregate": {"namespace"}, "accumulate": {"true"}}
	if err := c.getJSON(ctx, "/allocation", q, &parsed); err != nil {
		return 0, err
	}
	if len(parsed.Data) == 0 {
		return 0, nil
	}
	n := 0
	for name, a := range parsed.Data[0] {
		if name == "__idle__" {
			continue
		}
		if floatOrZero(a.TotalCost)+floatOrZero(a.CPUCost)+floatOrZero(a.RAMCost) > 0 {
			n++
		}
	}
	return n, nil
}

// Provider returns the cloud provider OpenCost detected for the cluster ("GCP", "AWS", "Azure",
// ... or "custom" when it found none and prices from its configured default rates).
func (c *Client) Provider(ctx context.Context) (string, error) {
	// The API wraps payloads as {"code":200,"data":{...}}; a bare object is accepted too.
	var parsed struct {
		Data     map[string]string `json:"data"`
		Provider string            `json:"provider"`
	}
	if err := c.getJSON(ctx, "/clusterInfo", nil, &parsed); err != nil {
		return "", err
	}
	if p := parsed.Data["provider"]; p != "" {
		return p, nil
	}
	return parsed.Provider, nil
}
