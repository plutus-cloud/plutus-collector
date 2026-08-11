// Package opencost is a thin client for OpenCost's in-cluster /allocation HTTP API.
//
// VERIFIED against github.com/opencost/opencost's own docs/swagger.json (develop branch,
// checked 2026-08-10) — the authoritative public API reference, not inferred from the Go
// struct's internal JSON tags (which differ: the raw Allocation struct has no totalCost field
// and stores PV cost as a per-volume "pvs" map; the API layer computes and flattens both before
// serializing). Confirmed from swagger.json's AllocationResponse/Allocation schemas:
//
//   - Endpoint is GET /allocation (there is no /allocation/compute route in the documented
//     API — an earlier draft of this client assumed that path and is corrected here).
//   - Response envelope: {"code": int, "status": string, "data": [ { "<key>": <Allocation>, ... } ]}
//     — "data" is an array of maps, one map per requested step (this client requests a single
//     one-day window, so it reads data[0]). Matches what was already implemented below.
//   - Allocation fields: cpuCost, ramCost, pvCost, networkCost, sharedCost, externalCost,
//     totalCost (all flat numbers), plus a "properties" object. Matches what was already
//     implemented below.
//   - Query parameters: window (required — "today"/"lastweek"/durations like "30m","7d"/RFC3339
//     pairs/Unix timestamps), aggregate (comma-separated field list, e.g. "namespace,controller"),
//     step (default: window), accumulate (bool). This client sends an RFC3339 pair for window,
//     which the docs confirm is accepted.
//
// STILL UNVERIFIED against swagger.json specifically (it documents "properties" only as a
// generic string-keyed map and doesn't enumerate cluster/namespace/controller/pod/container/
// labels as named sub-fields, and doesn't list gpuCost as an Allocation field at all — likely
// omissions in an intentionally-abbreviated reference doc rather than fields that don't exist,
// since the underlying Go struct (core/pkg/opencost/allocationprops.go,
// core/pkg/opencost/allocation.go) does define AllocationProperties.{Cluster, Namespace,
// ControllerKind, Controller, Pod, Container, Labels} and Allocation.GPUCost). This client
// still reads those fields optimistically (via pointers, defaulting to zero/empty when absent)
// so an instance that omits gpuCost or nests labels differently degrades gracefully rather than
// failing to parse — but this should be confirmed against a live /allocation response (e.g.
// `kubectl port-forward svc/opencost 9003:9003` against a real cluster, per swagger.json's own
// server description) before shipping.
//
// This is deliberately isolated behind the Client interface and the small Row struct so that
// any further correction only needs to change this file — internal/pusher and internal/ingest
// never see OpenCost's JSON directly.
package opencost

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Row is OpenCost's allocation output translated into the shape internal/ingest maps from. One
// Row per (cluster, namespace, controllerKind, controllerName, pod, container) — the same
// per-container granularity the CSV exporter produces (docs/kubernetes-cost-allocation.md §9.5).
type Row struct {
	Namespace      string
	Cluster        string
	ControllerKind string
	ControllerName string
	Pod            string
	Container      string

	CPUCost     float64
	RAMCost     float64
	NetworkCost float64
	PVCost      float64
	GPUCost     float64
	// TotalCost as OpenCost reports it. Can be zero/unset on some rows the same way the CSV
	// exporter's TotalCost cell can be blank (docs/kubernetes-cost-allocation.md §9.7) — callers
	// should prefer TotalCostOrSum() rather than this field directly.
	TotalCost    float64
	HasTotalCost bool

	Labels map[string]string
}

// TotalCostOrSum mirrors lib/sync/opencost.js's resolveRowCost: use the reported total when
// present, otherwise fall back to summing the component costs (exact, not an estimate, since
// the components sum to the total by construction).
func (r Row) TotalCostOrSum() float64 {
	if r.HasTotalCost {
		return r.TotalCost
	}
	return r.CPUCost + r.RAMCost + r.NetworkCost + r.PVCost + r.GPUCost
}

// Client queries an OpenCost instance's /allocation endpoint.
type Client struct {
	BaseURL    string
	HTTPClient *http.Client
}

// New builds a Client against baseURL (e.g. http://opencost.opencost.svc.cluster.local:9003),
// using httpClient for requests (pass one with a sane timeout — see config.HTTPTimeout).
func New(baseURL string, httpClient *http.Client) *Client {
	return &Client{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		HTTPClient: httpClient,
	}
}

// jsonAllocation is the ASSUMED wire shape of one OpenCost Allocation — see the package doc
// comment above for what's verified vs. assumed.
type jsonAllocation struct {
	Name       string `json:"name"`
	Properties struct {
		Cluster        string            `json:"cluster"`
		Namespace      string            `json:"namespace"`
		ControllerKind string            `json:"controllerKind"`
		Controller     string            `json:"controller"`
		Pod            string            `json:"pod"`
		Container      string            `json:"container"`
		Labels         map[string]string `json:"labels"`
	} `json:"properties"`

	CPUCost     *float64 `json:"cpuCost"`
	RAMCost     *float64 `json:"ramCost"`
	NetworkCost *float64 `json:"networkCost"`
	PVCost      *float64 `json:"pvCost"`
	GPUCost     *float64 `json:"gpuCost"`
	TotalCost   *float64 `json:"totalCost"`
}

type jsonResponse struct {
	Code int                         `json:"code"`
	Data []map[string]jsonAllocation `json:"data"`
	// OpenCost returns a non-empty message on error responses (assumed field name).
	Message string `json:"message"`
}

// ComputeAllocation queries /allocation for the single [start, end) window and returns
// one Row per allocation the API returns. start/end should be UTC day boundaries — see
// internal/pusher, which always requests "yesterday" (OpenCost's own aggregation is daily; see
// docs/kubernetes-cost-allocation.md §7).
func (c *Client) ComputeAllocation(ctx context.Context, start, end time.Time) ([]Row, error) {
	u := fmt.Sprintf(
		"%s/allocation?window=%s,%s&aggregate=namespace,controllerKind,controller,pod,container&accumulate=false",
		c.BaseURL,
		start.UTC().Format(time.RFC3339),
		end.UTC().Format(time.RFC3339),
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("building OpenCost request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("querying OpenCost at %s: %w", c.BaseURL, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20)) // 32MiB cap, defensive
	if err != nil {
		return nil, fmt.Errorf("reading OpenCost response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("OpenCost returned HTTP %d: %s", resp.StatusCode, truncate(string(body), 500))
	}

	var parsed jsonResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("parsing OpenCost response as JSON: %w (body: %s)", err, truncate(string(body), 500))
	}
	if parsed.Message != "" && len(parsed.Data) == 0 {
		return nil, fmt.Errorf("OpenCost reported an error: %s", parsed.Message)
	}
	if len(parsed.Data) == 0 {
		return nil, nil
	}

	rows := make([]Row, 0, len(parsed.Data[0]))
	for _, alloc := range parsed.Data[0] {
		rows = append(rows, Row{
			Namespace:      alloc.Properties.Namespace,
			Cluster:        alloc.Properties.Cluster,
			ControllerKind: alloc.Properties.ControllerKind,
			ControllerName: alloc.Properties.Controller,
			Pod:            alloc.Properties.Pod,
			Container:      alloc.Properties.Container,
			CPUCost:        floatOrZero(alloc.CPUCost),
			RAMCost:        floatOrZero(alloc.RAMCost),
			NetworkCost:    floatOrZero(alloc.NetworkCost),
			PVCost:         floatOrZero(alloc.PVCost),
			GPUCost:        floatOrZero(alloc.GPUCost),
			TotalCost:      floatOrZero(alloc.TotalCost),
			HasTotalCost:   alloc.TotalCost != nil,
			Labels:         alloc.Properties.Labels,
		})
	}
	return rows, nil
}

func floatOrZero(f *float64) float64 {
	if f == nil {
		return 0
	}
	return *f
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
