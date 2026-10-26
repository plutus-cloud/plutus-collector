package opencost

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const sampleResponse = `{
  "code": 200,
  "data": [
    {
      "checkout/deployment/checkout-api/checkout-api-abc123/app": {
        "name": "checkout/deployment/checkout-api/checkout-api-abc123/app",
        "properties": {
          "cluster": "prod-1",
          "namespace": "checkout",
          "controllerKind": "deployment",
          "controller": "checkout-api",
          "pod": "checkout-api-abc123",
          "container": "app",
          "labels": {"team": "platform"}
        },
        "cpuCost": 5.0,
        "ramCost": 4.0,
        "networkCost": 1.0,
        "pvCost": 2.0,
        "gpuCost": 0.34,
        "totalCost": 12.34
      },
      "kube-system/__unallocated__": {
        "name": "kube-system/__unallocated__",
        "properties": {
          "cluster": "prod-1",
          "namespace": "kube-system"
        },
        "cpuCost": 0.1,
        "ramCost": 0.2
      }
    }
  ]
}`

func TestComputeAllocation_ParsesSampleResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/allocation" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(sampleResponse))
	}))
	defer server.Close()

	c := New(server.URL, server.Client())
	rows, err := c.ComputeAllocation(context.Background(), time.Now().Add(-24*time.Hour), time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(rows))
	}

	var found bool
	for _, r := range rows {
		if r.Namespace == "checkout" {
			found = true
			if r.Cluster != "prod-1" {
				t.Errorf("expected cluster prod-1, got %q", r.Cluster)
			}
			if !r.HasTotalCost || r.TotalCostOrSum() != 12.34 {
				t.Errorf("expected totalCost 12.34, got %v (has=%v)", r.TotalCostOrSum(), r.HasTotalCost)
			}
			if r.Labels["team"] != "platform" {
				t.Errorf("expected label team=platform, got %v", r.Labels)
			}
		}
	}
	if !found {
		t.Error("expected to find the checkout namespace row")
	}
}

func TestComputeAllocation_TotalCostOrSumFallsBackWhenAbsent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(sampleResponse))
	}))
	defer server.Close()

	c := New(server.URL, server.Client())
	rows, err := c.ComputeAllocation(context.Background(), time.Now().Add(-24*time.Hour), time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, r := range rows {
		if r.Namespace == "kube-system" {
			if r.HasTotalCost {
				t.Error("expected kube-system row to have no reported totalCost")
			}
			if got, want := r.TotalCostOrSum(), 0.3; got < want-1e-9 || got > want+1e-9 {
				t.Errorf("expected component-sum fallback %v, got %v", want, got)
			}
		}
	}
}

func TestComputeAllocation_HTTPErrorSurfacesAsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	defer server.Close()

	c := New(server.URL, server.Client())
	_, err := c.ComputeAllocation(context.Background(), time.Now().Add(-24*time.Hour), time.Now())
	if err == nil {
		t.Fatal("expected an error for a 500 response")
	}
}
