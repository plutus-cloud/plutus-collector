// Package pusher runs a collector's main loop: on each tick, ask the configured Source for the
// previous UTC day's data, and push it with in-process retry/backoff. A failed push must never
// crash the process (design doc §2) — it's logged, counted in /metrics, and retried on the next
// tick.
//
// Source-agnostic by construction. It was OpenCost-specific until the LiteLLM collector was
// added; what made that a small change rather than a fork is that the loop's real subject is the
// RETRY AND FAILURE POLICY, not the data. Which errors are retryable, that a 4xx must not
// consume the backoff budget, that a 2xx with rejected rows is a partial failure the customer
// must see, that a cancelled context interrupts an in-progress backoff — those rules are
// identical for every source, and are exactly the rules that drift out of step when a second
// agent copies them.
package pusher

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/plutus-cloud/plutus-collector/internal/ingest"
	"github.com/plutus-cloud/plutus-collector/internal/metrics"
)

// Source produces one day's payload for whatever system this collector reads.
//
// Collect returns the fully-formed POST body, already in that source's ingest contract, plus the
// number of rows in it for metrics and logging. Building the payload is the source's job rather
// than the pusher's precisely so the pusher never has to know a field name — the previous version
// of this file assembled a Kubernetes batch inline, which is what made it a Kubernetes agent
// rather than a collector.
//
// A nil payload means "nothing to push": a real, non-error outcome (a gateway with no traffic
// yesterday), distinct from an error, and the pusher skips the push rather than sending an empty
// batch.
type Source interface {
	Collect(ctx context.Context, start, end time.Time, dateKey string) (payload any, rowCount int, err error)

	// Name identifies the source in log lines and error messages ("failed to query OpenCost").
	Name() string
}

// BatchPusher is the subset of ingest.Client's behavior the pusher needs — an interface so tests
// can substitute a fake instead of a real HTTP server.
type BatchPusher interface {
	Push(ctx context.Context, payload any) (*ingest.Response, error)
}

type Config struct {
	PushInterval   time.Duration
	MaxRetries     int
	RetryBaseDelay time.Duration
}

type Pusher struct {
	Source  Source
	Ingest  BatchPusher
	Metrics *metrics.State
	Config  Config
	Logger  *slog.Logger

	// Now and Sleep are overridable for tests; default to time.Now / time.Sleep.
	Now   func() time.Time
	Sleep func(time.Duration)
}

func New(source Source, ingestClient BatchPusher, m *metrics.State, cfg Config, logger *slog.Logger) *Pusher {
	if logger == nil {
		logger = slog.Default()
	}
	return &Pusher{
		Source:  source,
		Ingest:  ingestClient,
		Metrics: m,
		Config:  cfg,
		Logger:  logger,
		Now:     time.Now,
		Sleep:   time.Sleep,
	}
}

// Run blocks, ticking immediately and then every Config.PushInterval, until ctx is cancelled.
func (p *Pusher) Run(ctx context.Context) {
	p.tick(ctx)

	ticker := time.NewTicker(p.Config.PushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			p.Logger.Info("shutting down pusher loop", "reason", ctx.Err())
			return
		case <-ticker.C:
			p.tick(ctx)
		}
	}
}

func (p *Pusher) tick(ctx context.Context) {
	start := p.Now()
	windowStart, windowEnd, dateKey := PreviousUTCDayWindow(start)

	p.Logger.Info("starting push cycle", "source", p.Source.Name(), "date", dateKey, "window_start", windowStart, "window_end", windowEnd)

	payload, rowCount, err := p.Source.Collect(ctx, windowStart, windowEnd, dateKey)
	if err != nil {
		duration := p.Now().Sub(start)
		p.Metrics.RecordFailure(err, duration)
		p.Logger.Error("failed to query source", "source", p.Source.Name(), "error", err, "duration_seconds", duration.Seconds())
		return
	}

	// Distinct from an error, and deliberately not pushed as an empty batch: an empty batch
	// would make the backend delete the day's window and reinsert nothing, turning "we have no
	// data for you today" into "delete what you already have".
	if payload == nil || rowCount == 0 {
		p.Logger.Info("nothing to push for this window", "source", p.Source.Name(), "date", dateKey)
		p.Metrics.RecordSuccess(0, p.Now().Sub(start))
		return
	}

	p.Logger.Info("pushing batch", "source", p.Source.Name(), "date", dateKey, "row_count", rowCount)

	resp, err := p.pushWithRetry(ctx, payload)
	duration := p.Now().Sub(start)

	if err != nil {
		p.Metrics.RecordFailure(err, duration)
		p.Logger.Error("push failed after exhausting retries", "error", err, "duration_seconds", duration.Seconds(), "row_count", rowCount)
		return
	}

	p.Metrics.RecordSuccess(rowCount, duration)
	p.Logger.Info("push succeeded", "source", p.Source.Name(), "date", dateKey, "accepted", resp.Accepted, "rejected", resp.Rejected, "duration_seconds", duration.Seconds())

	// A 2xx with rejected rows is a partial failure the customer needs to see — this is their
	// only visibility into it (design doc §1), so every error string is logged individually
	// rather than just the count.
	if resp.Rejected > 0 {
		p.Logger.Warn("some rows were rejected by Plutus", "rejected", resp.Rejected, "accepted", resp.Accepted)
		for _, e := range resp.Errors {
			p.Logger.Warn("row rejection detail", "error", e)
		}
	}
}

// PreviousUTCDayWindow returns [start, end) for "yesterday" in UTC — the window every source in
// this repo is queried over. The UTC day is the unit because it is the only granularity that
// means the same thing across the systems Plutus reads (see plutus's CLAUDE.md on the cost
// horizon and redistribution), and because both OpenCost and LiteLLM aggregate daily anyway.
func PreviousUTCDayWindow(now time.Time) (start, end time.Time, dateKey string) {
	today := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day(), 0, 0, 0, 0, time.UTC)
	start = today.AddDate(0, 0, -1)
	end = today
	dateKey = start.Format("2006-01-02")
	return
}

// pushWithRetry retries a push on RetryableError with exponential backoff
// (RetryBaseDelay * 2^attempt), up to MaxRetries additional attempts after the first. A
// non-retryable error (bad request / bad key / suspended account — see ingest.Client.Push)
// returns immediately without consuming the retry budget, since retrying it can't succeed.
func (p *Pusher) pushWithRetry(ctx context.Context, payload any) (*ingest.Response, error) {
	var lastErr error

	for attempt := 0; attempt <= p.Config.MaxRetries; attempt++ {
		if attempt > 0 {
			delay := p.Config.RetryBaseDelay * time.Duration(1<<uint(attempt-1))
			p.Logger.Warn("retrying push", "attempt", attempt, "delay_seconds", delay.Seconds(), "last_error", lastErr)
			if err := p.sleepOrDone(ctx, delay); err != nil {
				return nil, err
			}
		}

		resp, err := p.Ingest.Push(ctx, payload)
		if err == nil {
			return resp, nil
		}

		var retryable *ingest.RetryableError
		if !errors.As(err, &retryable) {
			return nil, err
		}
		lastErr = err
	}

	return nil, lastErr
}

// sleepOrDone waits for delay via p.Sleep (kept as the overridable field so tests can still
// substitute a fast/no-op sleep), but returns as soon as ctx is cancelled instead of blocking
// out the full delay — a cancelled context must interrupt an in-progress backoff wait promptly,
// not just be checked once before it starts.
func (p *Pusher) sleepOrDone(ctx context.Context, delay time.Duration) error {
	done := make(chan struct{})
	go func() {
		p.Sleep(delay)
		close(done)
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
		return nil
	}
}
