package litellm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The endpoint and its envelope, both of which a live check corrected — see client.go's header.
// Plain /spend/logs returns a per-day rollup and ignores paging entirely, so hitting it would
// have looked fine on small data and silently re-read the same rows on a busy proxy.
func TestFetchSpendLogs_UsesTheV2EndpointAndItsEnvelope(t *testing.T) {
	var gotPath string
	var gotQuery []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = []string{r.URL.Query().Get("start_date"), r.URL.Query().Get("end_date"), r.URL.Query().Get("page")}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data":        []map[string]any{{"spend": 1.0, "model_group": "gpt-4o"}},
			"total":       1,
			"page":        1,
			"total_pages": 1,
		})
	}))
	defer server.Close()

	c := New(server.URL, "sk-master", server.Client())
	entries, err := c.FetchSpendLogs(context.Background(),
		time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotPath != "/spend/logs/v2" {
		t.Errorf("expected the paginated v2 route, got %q", gotPath)
	}
	if gotQuery[0] != "2026-06-01" || gotQuery[1] != "2026-06-02" {
		t.Errorf("expected YYYY-MM-DD bounds, got %v", gotQuery[:2])
	}
	if gotQuery[2] != "1" {
		t.Errorf("expected 1-based paging, got page=%q", gotQuery[2])
	}
	if len(entries) != 1 {
		t.Errorf("expected 1 entry from the envelope's data, got %d", len(entries))
	}
}

func TestFetchSpendLogs_PagesUntilTotalPages(t *testing.T) {
	var pages []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages = append(pages, r.URL.Query().Get("page"))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data":        []map[string]any{{"spend": 1.0}},
			"total_pages": 3,
		})
	}))
	defer server.Close()

	entries, err := New(server.URL, "sk-master", server.Client()).
		FetchSpendLogs(context.Background(), time.Now(), time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(pages) != 3 || len(entries) != 3 {
		t.Errorf("expected 3 pages and 3 entries, got pages=%v entries=%d", pages, len(entries))
	}
}

// total_pages missing or zero must not run the loop to MaxPages re-reading page 1.
func TestFetchSpendLogs_StopsWhenTotalPagesIsAbsent(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"spend": 1.0}}})
	}))
	defer server.Close()

	if _, err := New(server.URL, "sk-master", server.Client()).
		FetchSpendLogs(context.Background(), time.Now(), time.Now()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 1 {
		t.Errorf("expected a single request, got %d", calls)
	}
}

// The most likely misconfiguration, and the generic message sends people to the wrong place.
func TestFetchSpendLogs_ExplainsAnAdminScopeFailure(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
		}))
		_, err := New(server.URL, "sk-virtual", server.Client()).
			FetchSpendLogs(context.Background(), time.Now(), time.Now())
		server.Close()
		if err == nil {
			t.Fatalf("expected an error for HTTP %d", code)
		}
		if got := fmt.Sprint(err); !contains(got, "admin scope") {
			t.Errorf("HTTP %d error should mention admin scope, got: %v", code, got)
		}
	}
}

func contains(h, n string) bool {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return true
		}
	}
	return false
}
