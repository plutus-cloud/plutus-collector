package bifrost

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/plutus-cloud/plutus-collector/internal/config"
	"github.com/plutus-cloud/plutus-collector/internal/ingest"
	"github.com/plutus-cloud/plutus-collector/internal/metrics"
	"github.com/plutus-cloud/plutus-collector/internal/pusher"
)

var (
	day0 = time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	day1 = day0.AddDate(0, 0, 1)
)

// fakeBifrost serves the subset of Bifrost's API the collector reads, with the response shapes
// read from Bifrost's source (NOT a live instance). It honours the filters the way the real
// handler documents, unless ignoreTime is set to simulate a server that does not.
type fakeBifrost struct {
	mu         sync.Mutex
	logs       []map[string]any
	ignoreTime bool
	dropped    int64
	wantHeader string // header name that must carry wantValue, else 401
	wantValue  string
	requests   []string
}

func (f *fakeBifrost) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.requests = append(f.requests, r.URL.Path+"?"+r.URL.RawQuery)
		if f.wantHeader != "" && r.Header.Get(f.wantHeader) != f.wantValue {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		q := r.URL.Query()
		switch r.URL.Path {
		case "/api/logs/dropped":
			_ = json.NewEncoder(w).Encode(map[string]any{"dropped_requests": f.dropped})
		case "/api/logs", "/api/logs/stats":
			sel := f.filter(q)
			var cost float64
			for _, l := range sel {
				if c, ok := l["cost"].(float64); ok {
					cost += c
				}
			}
			stats := map[string]any{"total_requests": len(sel), "total_cost": cost}
			if r.URL.Path == "/api/logs/stats" {
				_ = json.NewEncoder(w).Encode(stats)
				return
			}
			limit, _ := strconv.Atoi(q.Get("limit"))
			offset, _ := strconv.Atoi(q.Get("offset"))
			if limit <= 0 || limit > 1000 {
				limit = 50
			}
			end := offset + limit
			if end > len(sel) {
				end = len(sel)
			}
			page := []map[string]any{}
			if offset < len(sel) {
				page = sel[offset:end]
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"logs": page, "pagination": map[string]any{"limit": limit, "offset": offset, "total_count": len(sel)},
				"stats": stats, "has_logs": true,
			})
		default:
			http.NotFound(w, r)
		}
	})
}

func (f *fakeBifrost) filter(q map[string][]string) []map[string]any {
	var start, end time.Time
	if v := q["start_time"]; len(v) > 0 {
		start, _ = time.Parse(time.RFC3339Nano, v[0])
	}
	if v := q["end_time"]; len(v) > 0 {
		end, _ = time.Parse(time.RFC3339Nano, v[0])
	}
	statusOnly := len(q["status"]) > 0 && q["status"][0] == "success"
	var out []map[string]any
	for _, l := range f.logs {
		ts, _ := time.Parse(time.RFC3339Nano, l["timestamp"].(string))
		if !f.ignoreTime && (ts.Before(start) || !ts.Before(end)) {
			continue
		}
		if statusOnly && l["status"] != "success" {
			continue
		}
		out = append(out, l)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i]["timestamp"].(string) < out[j]["timestamp"].(string) })
	return out
}

func rec(ts time.Time, status, provider, model, vk, team string, cost float64, in, out int) map[string]any {
	m := map[string]any{
		"timestamp": ts.Format(time.RFC3339Nano), "status": status, "provider": provider, "model": model,
		"cost": cost, "token_usage": map[string]any{"prompt_tokens": in, "completion_tokens": out, "total_tokens": in + out},
		// Content that must never be read into the output or logs.
		"input_history": []any{map[string]any{"content": "SECRET-PROMPT-TEXT"}},
	}
	if vk != "" {
		m["virtual_key_id"] = "id-" + vk
		m["virtual_key_name"] = vk
	}
	if team != "" {
		m["team_id"] = "tid-" + team
		m["team_name"] = team
	}
	return m
}

func newClient(srv *httptest.Server, auth Auth) *Client {
	return New(srv.URL, auth, DefaultFields(), srv.Client())
}

func TestFetchLogs_PagesAcrossThousandsAndFiltersToSuccess(t *testing.T) {
	f := &fakeBifrost{}
	for i := 0; i < 2300; i++ {
		f.logs = append(f.logs, rec(day0.Add(time.Duration(i)*time.Second), "success", "openai", "gpt-4o", "vk", "plat", 0.001, 1, 1))
	}
	f.logs = append(f.logs,
		rec(day0.Add(time.Hour), "error", "openai", "gpt-4o", "vk", "plat", 5, 1, 1),
		rec(day1.Add(time.Minute), "success", "openai", "gpt-4o", "vk", "plat", 7, 1, 1), // next day
	)
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	entries, outside, notOK, err := newClient(srv, Auth{}).FetchLogs(context.Background(), day0, day1)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2300 || outside != 0 || notOK != 0 {
		t.Errorf("want 2300 entries (error row and next-day row excluded server-side), got %d outside=%d notSuccess=%d", len(entries), outside, notOK)
	}
	logsReqs := 0
	for _, r := range f.requests {
		if strings.HasPrefix(r, "/api/logs?") {
			logsReqs++
			for _, want := range []string{"limit=1000", "status=success", "start_time=2026-10-16T00%3A00%3A00Z", "end_time=2026-10-17T00%3A00%3A00Z"} {
				if !strings.Contains(r, want) {
					t.Errorf("request %q missing %s", r, want)
				}
			}
		}
	}
	if logsReqs != 3 {
		t.Errorf("2300 records at 1000/page should take 3 requests, got %d", logsReqs)
	}
}

// A server that ignores start_time/end_time (or a mis-sent filter) would otherwise get another
// day's spend pushed under this day's date. The client checks timestamps itself.
func TestFetchLogs_SkipsRecordsOutsideTheWindowEvenIfServerIgnoresIt(t *testing.T) {
	f := &fakeBifrost{ignoreTime: true, logs: []map[string]any{
		rec(day0.Add(time.Hour), "success", "openai", "m", "vk", "t", 1, 1, 1),
		rec(day1.Add(time.Hour), "success", "openai", "m", "vk", "t", 100, 1, 1),
		rec(day0.Add(-time.Hour), "error", "openai", "m", "vk", "t", 100, 1, 1),
	}}
	f.logs[2]["status"] = "success"
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	entries, outside, _, err := newClient(srv, Auth{}).FetchLogs(context.Background(), day0, day1)
	if err != nil || len(entries) != 1 || outside != 2 {
		t.Fatalf("want 1 entry and 2 outside-window, got %d / %d err=%v", len(entries), outside, err)
	}
}

func TestFetchLogs_AuthHeaderAndBasic(t *testing.T) {
	f := &fakeBifrost{wantHeader: "X-Api-Key", wantValue: "k-123", logs: []map[string]any{rec(day0.Add(time.Hour), "success", "a", "m", "", "", 1, 1, 1)}}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	if _, _, _, err := newClient(srv, Auth{}).FetchLogs(context.Background(), day0, day1); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("expected a clear 401 error, got %v", err)
	} else if strings.Contains(err.Error(), "k-123") {
		t.Error("error must not contain a credential")
	}
	es, _, _, err := newClient(srv, Auth{Header: "X-Api-Key", Value: "k-123"}).FetchLogs(context.Background(), day0, day1)
	if err != nil || len(es) != 1 {
		t.Fatalf("authenticated fetch failed: %v %d", err, len(es))
	}

	var gotUser, gotPass string
	basic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPass, _ = r.BasicAuth()
		_, _ = io.WriteString(w, `{"logs":[]}`)
	}))
	defer basic.Close()
	_, _, _, _ = newClient(basic, Auth{BasicUser: "u", BasicPass: "p"}).FetchLogs(context.Background(), day0, day1)
	if gotUser != "u" || gotPass != "p" {
		t.Errorf("basic auth not sent: %q/%q", gotUser, gotPass)
	}
}

func TestExtract_DefensiveAndOverridable(t *testing.T) {
	c := New("http://x", Auth{}, DefaultFields(), nil)
	cases := map[string]string{
		"nested":       `{"provider":"openai","model":"m","cost":"1.5","token_usage":{"prompt_tokens":10,"completion_tokens":2}}`,
		"flat tokens":  `{"provider":"openai","model":"m","cost":1.5,"prompt_tokens":10,"completion_tokens":2}`,
		"alt names":    `{"provider":"openai","model":"m","cost":1.5,"token_usage":{"input_tokens":10,"output_tokens":2}}`,
		"null usage":   `{"provider":"openai","model":"m","cost":1.5,"token_usage":null}`,
		"garbage cost": `{"provider":"openai","cost":{"a":1}}`,
	}
	for name, raw := range cases {
		var r rawRecord
		if err := json.Unmarshal([]byte(raw), &r); err != nil {
			t.Fatal(err)
		}
		e := c.extract(r)
		switch name {
		case "nested", "flat tokens", "alt names":
			if e.Input != 10 || e.Output != 2 || e.Cost != 1.5 {
				t.Errorf("%s: %+v", name, e)
			}
		case "null usage":
			if e.Input != 0 || e.Cost != 1.5 {
				t.Errorf("%s: %+v", name, e)
			}
		case "garbage cost":
			if e.HasCost {
				t.Errorf("%s should have no cost: %+v", name, e)
			}
		}
	}
	// A virtual key / team with only an id falls back to the id; customer fills team when absent.
	var r rawRecord
	_ = json.Unmarshal([]byte(`{"virtual_key_id":"vk-1","customer_name":"acme"}`), &r)
	if e := c.extract(r); e.VirtualKey != "vk-1" || e.Team != "acme" {
		t.Errorf("fallbacks: %+v", e)
	}
	// Override.
	fm := DefaultFields()
	fm.Cost = []string{"usage.dollars"}
	_ = json.Unmarshal([]byte(`{"usage":{"dollars":9}}`), &r)
	if e := New("http://x", Auth{}, fm, nil).extract(r); e.Cost != 9 {
		t.Errorf("override: %+v", e)
	}
}

func TestAggregate(t *testing.T) {
	es := []Entry{
		{Provider: "openai", Model: "gpt", VirtualKey: "a", Team: "t", Cost: 1.5, Input: 10, Output: 1},
		{Provider: "openai", Model: "gpt", VirtualKey: "a", Team: "t", Cost: 2.5, Input: 5, Output: 2},
		{Provider: "bedrock", Model: "gpt", VirtualKey: "a", Team: "t", Cost: 1}, // provider is part of the key
		{Provider: "openai", Model: "unpriced", VirtualKey: "a", Team: "t", Input: 100},
		{Provider: "openai", Model: "empty", VirtualKey: "a", Team: "t"}, // nothing: dropped
		{Provider: "openai", Model: "nan", VirtualKey: "a", Team: "t", Cost: nanF()},
		{Model: "novk", Cost: 3}, // no provider/key/team: sent empty, backend buckets them
	}
	rows := Aggregate("2026-10-16", es)
	if len(rows) != 4 {
		t.Fatalf("want 4 rows, got %d: %+v", len(rows), rows)
	}
	if r := rows[0]; r.Spend != 4 || r.InputTokens != 15 || r.OutputTokens != 3 || r.Date != "2026-10-16" {
		t.Errorf("summing: %+v", r)
	}
	b, _ := json.Marshal(Batch{Rows: rows[:1]})
	want := `{"rows":[{"date":"2026-10-16","provider":"openai","model":"gpt","virtual_key":"a","team":"t","spend":4,"input_tokens":15,"output_tokens":3}]}`
	if string(b) != want {
		t.Errorf("wire shape drifted from the contract:\n got %s\nwant %s", b, want)
	}
}

func nanF() float64 { var z float64; return z / z }

type recordedPush struct {
	auth string
	body Batch
}

var mu sync.Mutex

func fakePlutus(t *testing.T, reply string, pushes *[]recordedPush) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b Batch
		_ = json.NewDecoder(r.Body).Decode(&b)
		mu.Lock()
		*pushes = append(*pushes, recordedPush{r.Header.Get("Authorization"), b})
		mu.Unlock()
		_, _ = io.WriteString(w, reply)
	}))
}

// The whole path: fake Bifrost -> Source -> pusher -> ingest client -> fake Plutus endpoint.
func TestEndToEnd_FakeBifrostToFakePlutus(t *testing.T) {
	prev := day0
	f := &fakeBifrost{dropped: 3}
	f.logs = []map[string]any{
		rec(prev.Add(time.Hour), "success", "openai", "gpt-4o", "checkout", "platform", 1.25, 100, 10),
		rec(prev.Add(2*time.Hour), "success", "openai", "gpt-4o", "checkout", "platform", 0.75, 50, 5),
		rec(prev.Add(3*time.Hour), "success", "bedrock", "claude", "search", "ml", 4, 10, 1),
		rec(prev.Add(4*time.Hour), "error", "openai", "gpt-4o", "checkout", "platform", 99, 1, 1),
	}
	bif := httptest.NewServer(f.handler())
	defer bif.Close()

	var pushes []recordedPush
	plutus := fakePlutus(t, `{"accepted":2,"usage_accepted":2,"rejected":0,"errors":[],"held_providers":["bedrock"]}`, &pushes)
	defer plutus.Close()

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))
	src := &Source{Logs: newClient(bif, Auth{}), Logger: logger}
	p := pusher.New(src, ingest.NewClient(plutus.URL, "plutus-key-XYZ", plutus.Client()), metrics.NewState(),
		pusher.Config{PushInterval: time.Hour, MaxRetries: 1, RetryBaseDelay: time.Millisecond}, logger)
	p.Now = func() time.Time { return day1.Add(5 * time.Hour) } // so "yesterday" is day0
	p.Sleep = func(time.Duration) {}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(pushes)
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done

	if len(pushes) != 1 {
		t.Fatalf("expected 1 push, got %d\nlogs: %s", len(pushes), logBuf.String())
	}
	got := pushes[0]
	if got.auth != "Bearer plutus-key-XYZ" {
		t.Errorf("push key header: %q", got.auth)
	}
	if len(got.body.Rows) != 2 {
		t.Fatalf("want 2 rows, got %+v", got.body.Rows)
	}
	if r := got.body.Rows[0]; r.Spend != 2 || r.Provider != "openai" || r.VirtualKey != "checkout" || r.Team != "platform" || r.InputTokens != 150 || r.Date != "2026-10-16" {
		t.Errorf("aggregated row: %+v", r)
	}

	logs := logBuf.String()
	for _, secret := range []string{"SECRET-PROMPT-TEXT", "plutus-key-XYZ"} {
		if strings.Contains(logs, secret) {
			t.Errorf("logs leaked %q", secret)
		}
	}
	// The error row is excluded by the status filter, so 3 records were read.
	for _, want := range []string{"held_providers", "dropped_requests", `"records":3`, "skipped_not_success"} {
		if !strings.Contains(logs, want) {
			t.Errorf("expected %q in logs:\n%s", want, logs)
		}
	}
}

func TestCollect_NoTrafficPushesNothing(t *testing.T) {
	f := &fakeBifrost{}
	bif := httptest.NewServer(f.handler())
	defer bif.Close()
	payload, n, err := (&Source{Logs: newClient(bif, Auth{})}).Collect(context.Background(), day0, day1, "2026-10-16")
	if payload != nil || n != 0 || err != nil {
		t.Errorf("expected nil payload, got %v %d %v", payload, n, err)
	}
}

func TestCollect_ReconciliationMismatchIsLoggedNotFatal(t *testing.T) {
	bif := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/logs":
			_, _ = fmt.Fprintf(w, `{"logs":[{"timestamp":%q,"status":"success","provider":"openai","model":"m","cost":1}]}`, day0.Add(time.Hour).Format(time.RFC3339))
		case "/api/logs/stats":
			_, _ = io.WriteString(w, `{"total_requests":5,"total_cost":10}`)
		default:
			w.WriteHeader(500)
		}
	}))
	defer bif.Close()
	var buf bytes.Buffer
	s := &Source{Logs: newClient(bif, Auth{}), Logger: slog.New(slog.NewJSONHandler(&buf, nil))}
	if _, n, err := s.Collect(context.Background(), day0, day1, "2026-10-16"); err != nil || n != 1 {
		t.Fatalf("collect: %d %v", n, err)
	}
	if !strings.Contains(buf.String(), "differs from Bifrost's own stats") {
		t.Errorf("expected a reconciliation warning:\n%s", buf.String())
	}
}

func TestFetchLogs_HTTPErrorDoesNotEchoBody(t *testing.T) {
	bif := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = io.WriteString(w, "SECRET-PROMPT-TEXT")
	}))
	defer bif.Close()
	_, _, _, err := newClient(bif, Auth{}).FetchLogs(context.Background(), day0, day1)
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Errorf("want an error without the body, got %v", err)
	}
}

func TestConfigHelpers(t *testing.T) {
	env := map[string]string{
		"PLUTUS_API_KEY": "k", "BIFROST_BASE_URL": "http://bifrost:8080", "BIFROST_API_KEY": "tok",
		"BIFROST_FIELD_COST": "usage.dollars, cost",
	}
	cfg, err := config.LoadBifrost(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if a := AuthFromConfig(cfg); a.Header != "Authorization" || a.Value != "Bearer tok" {
		t.Errorf("auth: %+v", a)
	}
	if f := FieldsFromConfig(cfg); len(f.Cost) != 2 || f.Cost[0] != "usage.dollars" || f.Provider[0] != "provider" {
		t.Errorf("fields: %+v", f)
	}
}
