package litellm

import (
	"context"
	"math"
	"testing"
	"time"
)

func entry(spend float64, model, provider, key, team string, in, out int64) LogEntry {
	return LogEntry{
		Spend: spend, Model: model, CustomLLMProvider: provider,
		APIKey: key, TeamID: team, PromptTokens: in, CompletionTokens: out,
	}
}

func findRow(rows []Row, model, key string) *Row {
	for i := range rows {
		if rows[i].Model == model && rows[i].VirtualKey == key {
			return &rows[i]
		}
	}
	return nil
}

// The core of the local reduction: request logs collapse to one row per
// (model, provider, key, team). This is what keeps per-request data on the customer's network.
func TestAggregate_CollapsesToTuples(t *testing.T) {
	rows := Aggregate("2026-06-01", []LogEntry{
		entry(1.5, "gpt-4o", "openai", "checkout", "platform", 100, 20),
		entry(2.5, "gpt-4o", "openai", "checkout", "platform", 50, 10),
		entry(4.0, "gpt-4o", "openai", "search", "platform", 10, 5),
	})

	if len(rows) != 2 {
		t.Fatalf("expected 2 tuples (two distinct keys), got %d: %+v", len(rows), rows)
	}
	checkout := findRow(rows, "gpt-4o", "checkout")
	if checkout == nil || checkout.Spend != 4.0 {
		t.Errorf("expected checkout spend 4.0, got %+v", checkout)
	}
	if checkout.InputTokens != 150 || checkout.OutputTokens != 30 {
		t.Errorf("expected token counts to sum, got in=%d out=%d", checkout.InputTokens, checkout.OutputTokens)
	}
}

// Every row carries the date it was aggregated for — the backend keys its window on it.
func TestAggregate_StampsTheDate(t *testing.T) {
	rows := Aggregate("2026-06-01", []LogEntry{entry(1, "m", "openai", "k", "t", 1, 1)})
	if rows[0].Date != "2026-06-01" {
		t.Errorf("expected the date key on the row, got %q", rows[0].Date)
	}
}

// LiteLLM's price map does not know every model, so an unpriced model reports real usage at zero
// cost. Dropping the row would discard the token counts, which are the half that does not depend
// on the price map being complete.
func TestAggregate_KeepsZeroSpendRowsThatCarryTokens(t *testing.T) {
	rows := Aggregate("2026-06-01", []LogEntry{entry(0, "new-model", "openai", "k", "t", 500, 100)})
	if len(rows) != 1 {
		t.Fatalf("expected the zero-spend row to survive, got %d rows", len(rows))
	}
	if rows[0].InputTokens != 500 {
		t.Errorf("expected token counts preserved, got %d", rows[0].InputTokens)
	}
}

func TestAggregate_DropsRowsWithNeitherSpendNorTokens(t *testing.T) {
	rows := Aggregate("2026-06-01", []LogEntry{entry(0, "m", "openai", "k", "t", 0, 0)})
	if len(rows) != 0 {
		t.Errorf("expected an empty row to be dropped, got %+v", rows)
	}
}

func TestAggregate_SkipsNaNAndInfSpend(t *testing.T) {
	for _, bad := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		rows := Aggregate("2026-06-01", []LogEntry{
			entry(bad, "m", "openai", "k", "t", 1, 1),
			entry(2.0, "m2", "openai", "k", "t", 1, 1),
		})
		// The bad entry is dropped; the good one in the same batch survives — a malformed
		// record must not cost the customer the whole day.
		if len(rows) != 1 || rows[0].Model != "m2" {
			t.Errorf("expected only the valid row to survive for %v, got %+v", bad, rows)
		}
	}
}

// Sub-cent daily totals are ordinary for a low-traffic key. Rounding to 2dp — as the Kubernetes
// collector does — would report them as having spent nothing.
func TestAggregate_PreservesSubCentSpend(t *testing.T) {
	rows := Aggregate("2026-06-01", []LogEntry{entry(0.004, "m", "openai", "k", "t", 1, 1)})
	if len(rows) != 1 || rows[0].Spend != 0.004 {
		t.Errorf("expected 0.004 preserved, got %+v", rows)
	}
}

func TestKeyAlias_PrefersReadableAliasOverHash(t *testing.T) {
	e := LogEntry{APIKey: "sk-hash-abc", Metadata: map[string]any{"user_api_key_alias": "checkout-svc"}}
	if got := e.KeyAlias(); got != "checkout-svc" {
		t.Errorf("expected the alias, got %q", got)
	}
	// A hash is a poor label but a stable identity — better than attributing the spend to nobody.
	bare := LogEntry{APIKey: "sk-hash-abc"}
	if got := bare.KeyAlias(); got != "sk-hash-abc" {
		t.Errorf("expected the key hash as fallback, got %q", got)
	}
}

type fakeLogs struct {
	entries []LogEntry
	err     error
}

func (f *fakeLogs) FetchSpendLogs(ctx context.Context, start, end time.Time) ([]LogEntry, error) {
	return f.entries, f.err
}

// A gateway with no traffic yesterday must produce nil, not an empty Batch: the pusher treats
// nil as "nothing to push", and an empty batch would make the backend delete the day's window
// and reinsert nothing.
func TestSource_ReturnsNilWhenThereIsNothingToPush(t *testing.T) {
	s := &Source{Logs: &fakeLogs{}}
	payload, count, err := s.Collect(context.Background(), time.Now(), time.Now(), "2026-06-01")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if payload != nil || count != 0 {
		t.Errorf("expected a nil payload for an empty day, got %+v (%d rows)", payload, count)
	}
}

func TestSource_ReturnsBatchAndRowCount(t *testing.T) {
	s := &Source{Logs: &fakeLogs{entries: []LogEntry{
		entry(1, "gpt-4o", "openai", "k1", "t", 1, 1),
		entry(1, "claude-sonnet-5", "anthropic", "k2", "t", 1, 1),
	}}}
	payload, count, err := s.Collect(context.Background(), time.Now(), time.Now(), "2026-06-01")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 2 {
		t.Errorf("expected 2 rows, got %d", count)
	}
	if _, ok := payload.(Batch); !ok {
		t.Errorf("expected a Batch payload, got %T", payload)
	}
}
