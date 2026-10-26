package k8scost

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/plutus-cloud/plutus-collector/internal/opencost"
	"github.com/plutus-cloud/plutus-collector/internal/pusher"
)

func TestSource_EmptyWindowCarriesAReasonAndSerialisesRowsAsAnArray(t *testing.T) {
	for _, tc := range []struct {
		name   string
		rows   []opencost.Row
		reason string
	}{
		{"opencost returned nothing", nil, EmptyNoAllocations},
		{"everything priced at zero", []opencost.Row{{Namespace: "idle"}}, EmptyAllZeroCost},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Source{Allocations: &fakeAllocations{rows: tc.rows}, ClusterName: "c", Currency: "USD"}
			payload, count, err := s.Collect(context.Background(), time.Now(), time.Now(), "2026-06-01")
			if err != nil || count != 0 {
				t.Fatalf("count=%d err=%v", count, err)
			}
			batch := payload.(Batch)
			if batch.Collector == nil || batch.Collector.EmptyReason != tc.reason {
				t.Errorf("empty reason = %+v, want %s", batch.Collector, tc.reason)
			}
			body, _ := json.Marshal(batch)
			if !strings.Contains(string(body), `"rows":[]`) {
				t.Errorf("rows must serialise as an empty array, got %s", body)
			}
		})
	}
}

// The contract with the backend: a heartbeat is {cluster_name, currency, rows: [], collector:
// {version, prometheus_mode, window:{start,end}, empty_reason, preflight}}.
func TestSource_AnnotateProducesTheHeartbeatContract(t *testing.T) {
	s := &Source{Allocations: &fakeAllocations{}, ClusterName: "c", Currency: "USD"}
	payload, _, _ := s.Collect(context.Background(), time.Now(), time.Now(), "2026-06-01")

	start := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	out := s.Annotate(payload, pusher.CollectorMeta{
		Version: "v1.3.0", PrometheusMode: "bundled",
		WindowStart: start, WindowEnd: start.AddDate(0, 0, 1),
		Preflight: map[string]string{"opencost_series": "error"},
	})
	body, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got["cluster_name"] != "c" || got["currency"] != "USD" {
		t.Errorf("header lost: %s", body)
	}
	if rows, ok := got["rows"].([]any); !ok || len(rows) != 0 {
		t.Errorf("rows must be an empty array: %s", body)
	}
	c, _ := got["collector"].(map[string]any)
	if c["version"] != "v1.3.0" || c["prometheus_mode"] != "bundled" || c["empty_reason"] != EmptyNoAllocations {
		t.Errorf("collector block wrong: %s", body)
	}
	w, _ := c["window"].(map[string]any)
	if w["start"] != "2026-06-01T00:00:00Z" || w["end"] != "2026-06-02T00:00:00Z" {
		t.Errorf("window wrong: %v", w)
	}
	if pf, _ := c["preflight"].(map[string]any); pf["opencost_series"] != "error" {
		t.Errorf("preflight lost: %v", c["preflight"])
	}
}

func TestSource_AnnotateOnAPushWithRowsOmitsEmptyReason(t *testing.T) {
	s := &Source{Allocations: &fakeAllocations{rows: []opencost.Row{{Namespace: "a", TotalCost: 1, HasTotalCost: true}}}, ClusterName: "c", Currency: "USD"}
	payload, count, _ := s.Collect(context.Background(), time.Now(), time.Now(), "2026-06-01")
	if count != 1 {
		t.Fatalf("count = %d", count)
	}
	body, _ := json.Marshal(s.Annotate(payload, pusher.CollectorMeta{Version: "v1"}))
	if strings.Contains(string(body), "empty_reason") {
		t.Errorf("a push with rows must not carry an empty reason: %s", body)
	}
}

func TestSource_EmptyPolicyFollowsEmptyOK(t *testing.T) {
	if (&Source{}).EmptyIsHealthy() || !(&Source{EmptyOK: true}).EmptyIsHealthy() {
		t.Error("EmptyIsHealthy must mirror EmptyOK")
	}
}
