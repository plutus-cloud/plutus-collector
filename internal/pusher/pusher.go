// Package pusher runs the agent's main loop: on each tick, query OpenCost for the previous
// UTC day's allocation, map it into Plutus's ingest batch shape, and push it with in-process
// retry/backoff. A failed push must never crash the process (design doc §2) — it's logged,
// counted in /metrics, and retried on the next tick.
package pusher

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/plutus-cloud/plutus-collector/internal/ingest"
	"github.com/plutus-cloud/plutus-collector/internal/metrics"
	"github.com/plutus-cloud/plutus-collector/internal/opencost"
)

// AllocationSource is the subset of opencost.Client's behavior the pusher needs — an interface
// so tests can substitute a fake instead of a real OpenCost HTTP server.
type AllocationSource interface {
	ComputeAllocation(ctx context.Context, start, end time.Time) ([]opencost.Row, error)
}

// BatchPusher is the subset of ingest.Client's behavior the pusher needs, for the same reason.
type BatchPusher interface {
	Push(ctx context.Context, batch ingest.Batch) (*ingest.Response, error)
}

type Config struct {
	ClusterName    string
	Currency       string
	PushInterval   time.Duration
	MaxRetries     int
	RetryBaseDelay time.Duration
}

type Pusher struct {
	OpenCost AllocationSource
	Ingest   BatchPusher
	Metrics  *metrics.State
	Config   Config
	Logger   *slog.Logger

	// Now and Sleep are overridable for tests; default to time.Now / time.Sleep.
	Now   func() time.Time
	Sleep func(time.Duration)
}

func New(source AllocationSource, ingestClient BatchPusher, m *metrics.State, cfg Config, logger *slog.Logger) *Pusher {
	if logger == nil {
		logger = slog.Default()
	}
	return &Pusher{
		OpenCost: source,
		Ingest:   ingestClient,
		Metrics:  m,
		Config:   cfg,
		Logger:   logger,
		Now:      time.Now,
		Sleep:    time.Sleep,
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
	windowStart, windowEnd, dateKey := ingest.PreviousUTCDayWindow(start)

	p.Logger.Info("starting push cycle", "date", dateKey, "window_start", windowStart, "window_end", windowEnd)

	rows, err := p.OpenCost.ComputeAllocation(ctx, windowStart, windowEnd)
	if err != nil {
		duration := p.Now().Sub(start)
		p.Metrics.RecordFailure(err, duration)
		p.Logger.Error("failed to query OpenCost", "error", err, "duration_seconds", duration.Seconds())
		return
	}

	ocRows := make([]ingest.OpenCostRow, 0, len(rows))
	for _, r := range rows {
		ocRows = append(ocRows, ingest.OpenCostRow{
			Namespace:      r.Namespace,
			Cluster:        r.Cluster,
			ControllerKind: r.ControllerKind,
			ControllerName: r.ControllerName,
			Pod:            r.Pod,
			Container:      r.Container,
			CPUCost:        r.CPUCost,
			RAMCost:        r.RAMCost,
			NetworkCost:    r.NetworkCost,
			PVCost:         r.PVCost,
			GPUCost:        r.GPUCost,
			TotalCost:      r.TotalCostOrSum(),
			Labels:         r.Labels,
		})
	}

	batch := ingest.Batch{
		ClusterName: p.Config.ClusterName,
		Currency:    p.Config.Currency,
		Rows:        ingest.MapRows(dateKey, p.Config.ClusterName, ocRows),
	}

	p.Logger.Info("pushing batch", "date", dateKey, "row_count", len(batch.Rows))

	resp, err := p.pushWithRetry(ctx, batch)
	duration := p.Now().Sub(start)

	if err != nil {
		p.Metrics.RecordFailure(err, duration)
		p.Logger.Error("push failed after exhausting retries", "error", err, "duration_seconds", duration.Seconds(), "row_count", len(batch.Rows))
		return
	}

	p.Metrics.RecordSuccess(len(batch.Rows), duration)
	p.Logger.Info("push succeeded", "date", dateKey, "accepted", resp.Accepted, "rejected", resp.Rejected, "duration_seconds", duration.Seconds())

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

// pushWithRetry retries a push on RetryableError with exponential backoff
// (RetryBaseDelay * 2^attempt), up to MaxRetries additional attempts after the first. A
// non-retryable error (bad request / bad key / suspended account — see ingest.Client.Push)
// returns immediately without consuming the retry budget, since retrying it can't succeed.
func (p *Pusher) pushWithRetry(ctx context.Context, batch ingest.Batch) (*ingest.Response, error) {
	var lastErr error

	for attempt := 0; attempt <= p.Config.MaxRetries; attempt++ {
		if attempt > 0 {
			delay := p.Config.RetryBaseDelay * time.Duration(1<<uint(attempt-1))
			p.Logger.Warn("retrying push", "attempt", attempt, "delay_seconds", delay.Seconds(), "last_error", lastErr)
			if err := p.sleepOrDone(ctx, delay); err != nil {
				return nil, err
			}
		}

		resp, err := p.Ingest.Push(ctx, batch)
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
