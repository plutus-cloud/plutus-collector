package opencost

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func probeClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return New(srv.URL, &http.Client{Timeout: 5 * time.Second})
}

func TestCountAllocations_IgnoresIdleAndZeroCostEntries(t *testing.T) {
	c := probeClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/allocation" || r.URL.Query().Get("window") != "1h" {
			t.Errorf("unexpected request %s", r.URL)
		}
		_, _ = w.Write([]byte(`{"code":200,"data":[{
			"kube-system":{"totalCost":0.4},
			"checkout":{"cpuCost":0.2},
			"__idle__":{"totalCost":3},
			"empty":{"totalCost":0}
		}]}`))
	})
	n, err := c.CountAllocations(context.Background(), "1h")
	if err != nil || n != 2 {
		t.Errorf("n=%d err=%v, want 2 priced namespaces", n, err)
	}
}

func TestCountAllocations_NoDataIsZeroNotAnError(t *testing.T) {
	c := probeClient(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"code":200,"data":[]}`)) })
	if n, err := c.CountAllocations(context.Background(), "1h"); err != nil || n != 0 {
		t.Errorf("n=%d err=%v", n, err)
	}
}

func TestProvider_ReadsTheWrappedClusterInfo(t *testing.T) {
	c := probeClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":200,"data":{"provider":"custom","name":"x"}}`))
	})
	if p, err := c.Provider(context.Background()); err != nil || p != "custom" {
		t.Errorf("provider=%q err=%v", p, err)
	}
}

func TestProbe_ReportsHTTPFailure(t *testing.T) {
	c := probeClient(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "starting", http.StatusServiceUnavailable) })
	if _, err := c.Provider(context.Background()); err == nil {
		t.Error("expected an error for a 503")
	}
}
