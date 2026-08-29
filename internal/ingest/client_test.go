package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClient_Push_Success(t *testing.T) {
	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(Response{Accepted: 2, Rejected: 0})
	}))
	defer server.Close()

	c := NewClient(server.URL, "test-key", server.Client())
	resp, err := c.Push(context.Background(), map[string]any{"cluster_name": "prod-1", "currency": "USD"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Accepted != 2 {
		t.Errorf("expected accepted=2, got %d", resp.Accepted)
	}
	if gotAuth != "Bearer test-key" {
		t.Errorf("expected Authorization header 'Bearer test-key', got %q", gotAuth)
	}
}

func TestClient_Push_5xxIsRetryable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("temporarily unavailable"))
	}))
	defer server.Close()

	c := NewClient(server.URL, "test-key", server.Client())
	_, err := c.Push(context.Background(), struct{}{})

	var retryable *RetryableError
	if !errors.As(err, &retryable) {
		t.Fatalf("expected a RetryableError for 5xx, got %v (%T)", err, err)
	}
}

func TestClient_Push_4xxIsNotRetryable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("invalid api key"))
	}))
	defer server.Close()

	c := NewClient(server.URL, "bad-key", server.Client())
	_, err := c.Push(context.Background(), struct{}{})

	var retryable *RetryableError
	if errors.As(err, &retryable) {
		t.Fatalf("expected a non-retryable error for 4xx, got RetryableError: %v", err)
	}
	if err == nil {
		t.Fatal("expected an error for 401 response")
	}
}

func TestClient_Push_NetworkFailureIsRetryable(t *testing.T) {
	// Point at a URL nothing is listening on.
	c := NewClient("http://127.0.0.1:0", "test-key", http.DefaultClient)
	_, err := c.Push(context.Background(), struct{}{})

	var retryable *RetryableError
	if !errors.As(err, &retryable) {
		t.Fatalf("expected a RetryableError for a network failure, got %v (%T)", err, err)
	}
}

// The transport must serialise whatever it is handed and nothing more — it has no knowledge of
// any source's payload shape, which is what lets both collectors in this repo share it.
func TestPush_SendsThePayloadVerbatim(t *testing.T) {
	var gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(Response{Accepted: 1})
	}))
	defer server.Close()

	type litellmish struct {
		Rows []map[string]any `json:"rows"`
	}
	c := NewClient(server.URL, "test-key", server.Client())
	if _, err := c.Push(context.Background(), litellmish{Rows: []map[string]any{{"model": "gpt-4o"}}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotBody != `{"rows":[{"model":"gpt-4o"}]}` {
		t.Errorf("payload was not sent verbatim, got %s", gotBody)
	}
}
