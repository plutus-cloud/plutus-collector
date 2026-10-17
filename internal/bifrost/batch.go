package bifrost

import (
	"context"
	"log/slog"
	"math"
	"time"
)

// Row is one (provider, model, virtual key, team-or-customer) tuple's spend and tokens for one
// UTC day, matching the field names of Plutus's bifrost-cost ingest endpoint. Do not rename a
// field: the endpoint ignores unknown fields, so a rename would silently drop that dimension.
type Row struct {
	Date         string  `json:"date"`
	Provider     string  `json:"provider"`
	Model        string  `json:"model"`
	VirtualKey   string  `json:"virtual_key"`
	Team         string  `json:"team"`
	Spend        float64 `json:"spend"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
}

// Batch is the POST body. No currency: Bifrost reports USD and the backend asserts it, as with
// LiteLLM.
type Batch struct {
	Rows []Row `json:"rows"`
}

// Aggregate reduces request logs to one row per tuple for one UTC day. Provider is passed through
// verbatim (the backend keys a per-provider decision on it, so it must not be normalised).
// Rows with neither spend nor tokens are dropped; tokens-without-spend are kept (unpriced models).
func Aggregate(dateKey string, entries []Entry) []Row {
	type key struct{ provider, model, vk, team string }
	agg := map[key]*Row{}
	var order []key
	for _, e := range entries {
		if !validCost(e.Cost) {
			continue
		}
		k := key{e.Provider, e.Model, e.VirtualKey, e.Team}
		row, ok := agg[k]
		if !ok {
			row = &Row{Date: dateKey, Provider: k.provider, Model: k.model, VirtualKey: k.vk, Team: k.team}
			agg[k] = row
			order = append(order, k)
		}
		row.Spend += e.Cost
		row.InputTokens += e.Input
		row.OutputTokens += e.Output
	}
	out := make([]Row, 0, len(agg))
	for _, k := range order { // first-seen order keeps output deterministic
		r := agg[k]
		if r.Spend == 0 && r.InputTokens == 0 && r.OutputTokens == 0 {
			continue
		}
		r.Spend = math.Round(r.Spend*1e6) / 1e6
		out = append(out, *r)
	}
	return out
}

func validCost(f float64) bool {
	return !math.IsNaN(f) && !math.IsInf(f, 0) && math.Abs(f) < float64(math.MaxInt64)/1e6
}

// LogSource is what Source needs from the client, so tests can substitute a fake.
type LogSource interface {
	FetchLogs(ctx context.Context, start, end time.Time) (entries []Entry, outsideWindow, notSuccess int, err error)
	FetchStats(ctx context.Context, start, end time.Time) (*Stats, error)
	FetchDropped(ctx context.Context) (int64, error)
}

// Source adapts Bifrost to the pusher's Source interface.
type Source struct {
	Logs   LogSource
	Logger *slog.Logger
}

func (s *Source) Name() string { return "Bifrost" }

func (s *Source) Collect(ctx context.Context, start, end time.Time, dateKey string) (any, int, error) {
	entries, outside, notSuccess, err := s.Logs.FetchLogs(ctx, start, end)
	if err != nil {
		return nil, 0, err
	}
	rows := Aggregate(dateKey, entries)

	if s.Logger != nil {
		var sum float64
		for _, r := range rows {
			sum += r.Spend
		}
		s.Logger.Info("read Bifrost logs", "date", dateKey, "records", len(entries), "rows", len(rows),
			"skipped_not_success", notSuccess, "skipped_outside_window", outside, "spend_sum", round6(sum))
		if outside > 0 {
			s.Logger.Warn("Bifrost returned records outside the requested day; they were skipped — the start_time/end_time filter may not be honoured by this version", "count", outside)
		}
		s.reconcile(ctx, start, end, dateKey, sum)
	}

	if len(rows) == 0 {
		// nil, not an empty Batch: an empty batch would make the backend delete the day's rows
		// and write nothing back. Note this also covers a day whose logs Bifrost has already
		// purged under its retention policy — the existing Plutus rows are left alone.
		return nil, 0, nil
	}
	return Batch{Rows: rows}, len(rows), nil
}

// reconcile is best effort and advisory: it logs, and never fails the push.
func (s *Source) reconcile(ctx context.Context, start, end time.Time, dateKey string, sum float64) {
	if st, err := s.Logs.FetchStats(ctx, start, end); err != nil {
		s.Logger.Info("could not read Bifrost /api/logs/stats for reconciliation", "error", err)
	} else if diff := math.Abs(st.TotalCost - sum); diff > 0.01 && diff > 0.005*math.Abs(st.TotalCost) {
		s.Logger.Warn("collector's spend differs from Bifrost's own stats total for the day", "date", dateKey,
			"collector_spend", round6(sum), "bifrost_total_cost", round6(st.TotalCost), "bifrost_total_requests", st.TotalRequests)
	}
	if d, err := s.Logs.FetchDropped(ctx); err != nil {
		s.Logger.Info("could not read Bifrost /api/logs/dropped", "error", err)
	} else if d > 0 {
		s.Logger.Warn("Bifrost reports requests it failed to log; spend is undercounted", "dropped_requests", d)
	}
}

func round6(f float64) float64 { return math.Round(f*1e6) / 1e6 }
