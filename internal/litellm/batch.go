package litellm

import (
	"context"
	"math"
	"time"
)

// Row is one (model, provider, virtual key, team) tuple's spend and token counts for one UTC
// day, matching plutus-backend's routes/litellm-cost-ingest.js field-for-field. Do not rename a
// field without coordinating there.
//
// The tuple is the unit because the backend writes each row into four grains — model, upstream
// provider, virtual key, team — and every grain has to sum to the same daily total. See this
// package's client.go header for why that rules out LiteLLM's own per-dimension daily endpoints.
type Row struct {
	Date         string  `json:"date"`
	Model        string  `json:"model"`
	Provider     string  `json:"provider"`
	VirtualKey   string  `json:"virtual_key"`
	Team         string  `json:"team"`
	Spend        float64 `json:"spend"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
}

// Batch is the full POST body.
//
// No currency field, deliberately, and this is the one place the two collectors in this repo
// differ on a point of principle rather than of data. The Kubernetes batch must carry a currency
// because OpenCost emits none and its operator configures it; LiteLLM prices from a
// USD-denominated model price map, so the backend asserts USD itself. A collector-supplied
// currency here would be a value nobody set and nobody checked.
type Batch struct {
	Rows []Row `json:"rows"`
}

// Aggregate reduces request-level spend logs to one row per (model, provider, key, team) for a
// single UTC day.
//
// This is the reduction that keeps per-request data inside the customer's network. It runs on
// their side of the wire; only its output is pushed.
//
// Rows with neither spend nor tokens are dropped — nothing informative, and they only inflate
// the row count against the backend's batch cap. A row with tokens and no spend is KEPT: LiteLLM
// prices from a map that does not know every model, so an unpriced model reports real usage at
// zero cost, and the token counts are the half that does not depend on the price map being
// complete.
func Aggregate(dateKey string, entries []LogEntry) []Row {
	type key struct{ model, provider, virtualKey, team string }
	agg := make(map[key]*Row)

	for _, e := range entries {
		if !isValidCost(e.Spend) {
			// A NaN/Inf spend would poison the whole tuple's total and serialise as invalid
			// JSON. Drop the entry rather than the day.
			continue
		}
		k := key{
			model:      e.Model,
			provider:   e.CustomLLMProvider,
			virtualKey: e.KeyAlias(),
			team:       e.TeamAlias(),
		}
		row, ok := agg[k]
		if !ok {
			row = &Row{
				Date:       dateKey,
				Model:      k.model,
				Provider:   k.provider,
				VirtualKey: k.virtualKey,
				Team:       k.team,
			}
			agg[k] = row
		}
		row.Spend += e.Spend
		row.InputTokens += e.PromptTokens
		row.OutputTokens += e.CompletionTokens
	}

	out := make([]Row, 0, len(agg))
	for _, row := range agg {
		if row.Spend == 0 && row.InputTokens == 0 && row.OutputTokens == 0 {
			continue
		}
		row.Spend = round6(row.Spend)
		out = append(out, *row)
	}
	return out
}

// round6, not the Kubernetes collector's round2. Per-token pricing routinely produces genuine
// sub-cent daily totals for a low-traffic key, and rounding those to 2dp would silently zero
// them — the collector would report a key as having spent nothing when it spent $0.004. Six
// places is comfortably finer than any real figure while still cutting float noise.
func round6(f float64) float64 {
	return math.Round(f*1e6) / 1e6
}

func isValidCost(f float64) bool {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return false
	}
	const maxUnits = float64(math.MaxInt64) / 1e6
	return f > -maxUnits && f < maxUnits
}

// LogSource is the subset of Client's behavior Source needs, so tests can substitute a fake.
type LogSource interface {
	FetchSpendLogs(ctx context.Context, start, end time.Time) ([]LogEntry, error)
}

// Source adapts LiteLLM to the pusher's Source interface.
type Source struct {
	Logs LogSource
}

func (s *Source) Name() string { return "LiteLLM" }

func (s *Source) Collect(ctx context.Context, start, end time.Time, dateKey string) (any, int, error) {
	entries, err := s.Logs.FetchSpendLogs(ctx, start, end)
	if err != nil {
		return nil, 0, err
	}
	rows := Aggregate(dateKey, entries)
	if len(rows) == 0 {
		// nil, not an empty Batch — the pusher treats that as "nothing to push" and skips the
		// request, rather than sending a batch whose window-delete would remove the day's
		// existing rows and reinsert nothing.
		return nil, 0, nil
	}
	return Batch{Rows: rows}, len(rows), nil
}
