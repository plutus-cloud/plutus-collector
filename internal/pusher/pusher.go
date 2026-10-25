// Package pusher runs a collector's main loop: on each tick, ask the configured Source for the
// recent closed UTC days' data, and push it with in-process retry/backoff. A failed push must
// never crash the process (design doc §2) — it's logged, counted in /metrics, and retried on the
// next tick.
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
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/plutus-cloud/plutus-collector/internal/ingest"
	"github.com/plutus-cloud/plutus-collector/internal/metrics"
)

// Source produces one window's payload for whatever system this collector reads.
//
// Collect returns the fully-formed POST body, already in that source's ingest contract, plus the
// number of rows in it for metrics and logging. Building the payload is the source's job rather
// than the pusher's precisely so the pusher never has to know a field name — the previous version
// of this file assembled a Kubernetes batch inline, which is what made it a Kubernetes agent
// rather than a collector.
//
// A rowCount of zero means the window produced nothing. Whether that is normal is the source's
// policy (EmptyIsHealthy), not the pusher's: a gateway with no traffic yesterday is fine, while a
// Kubernetes cluster always has at least its own system pods, so an empty allocation result
// means the data pipeline behind OpenCost is broken.
type Source interface {
	Collect(ctx context.Context, start, end time.Time, dateKey string) (payload any, rowCount int, err error)

	// Name identifies the source in log lines and error messages ("failed to query OpenCost").
	Name() string

	// EmptyIsHealthy reports whether a window with no rows is a normal outcome for this source.
	EmptyIsHealthy() bool
}

// CollectorMeta is what the agent reports about itself and the window alongside a push.
type CollectorMeta struct {
	Version        string
	PrometheusMode string
	WindowStart    time.Time
	WindowEnd      time.Time
	// Preflight is check id -> "ok" | "warn" | "error"; empty until preflight has run.
	Preflight map[string]string
}

// Annotator is implemented by sources whose ingest endpoint accepts a `collector` block. It also
// marks the source as one that sends a heartbeat for an empty window: an empty batch is only safe
// to POST to an endpoint that treats rows: [] as "write nothing", which is a property of the
// endpoint and not something the pusher can assume for every source.
//
// Annotate returns payload with the block filled in, keeping anything the source already put in
// it (an empty reason, for instance).
type Annotator interface {
	Annotate(payload any, meta CollectorMeta) any
}

// BatchPusher is the subset of ingest.Client's behavior the pusher needs — an interface so tests
// can substitute a fake instead of a real HTTP server.
type BatchPusher interface {
	Push(ctx context.Context, payload any) (*ingest.Response, error)
}

// Schedule fires once a day at a fixed UTC time plus a random offset. A fixed time instead of
// "process start + 24h" keeps the push after the day's data is complete however often the pod is
// rescheduled, and the offset keeps a fleet of installs from all hitting the ingest endpoint in
// the same second.
type Schedule struct {
	Hour, Minute int
	Jitter       time.Duration
}

// Next returns the first scheduled time strictly after now. rnd returns a value in [0, n) and is
// a parameter so tests are deterministic.
func (s Schedule) Next(now time.Time, rnd func(n int64) int64) time.Time {
	now = now.UTC()
	offset := time.Duration(0)
	if secs := int64(s.Jitter / time.Second); secs > 0 {
		offset = time.Duration(rnd(secs+1)) * time.Second
	}
	at := time.Date(now.Year(), now.Month(), now.Day(), s.Hour, s.Minute, 0, 0, time.UTC).Add(offset)
	if !at.After(now) {
		at = time.Date(now.Year(), now.Month(), now.Day()+1, s.Hour, s.Minute, 0, 0, time.UTC).Add(offset)
	}
	return at
}

type Config struct {
	// PushInterval drives the loop when Schedule is nil (the legacy fixed-interval behaviour).
	PushInterval time.Duration
	Schedule     *Schedule

	// CatchUpDays is how many closed UTC days each tick covers. Values below 1 mean 1.
	CatchUpDays int

	MaxRetries     int
	RetryBaseDelay time.Duration

	// Version and PrometheusMode are reported in the collector block.
	Version        string
	PrometheusMode string
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
	// Rand returns a value in [0, n); defaults to math/rand/v2.
	Rand func(n int64) int64

	// pushed holds the dates this process has already delivered with rows. A catch-up tick skips
	// them: re-querying a day that was already delivered gains nothing and, if the metrics store
	// has since lost history, could replace complete data with a partial day.
	pushed map[string]bool
	// emptyDays records, per date, whether the last attempt came back empty.
	emptyDays map[string]bool
}

func New(source Source, ingestClient BatchPusher, m *metrics.State, cfg Config, logger *slog.Logger) *Pusher {
	if logger == nil {
		logger = slog.Default()
	}
	return &Pusher{
		Source:    source,
		Ingest:    ingestClient,
		Metrics:   m,
		Config:    cfg,
		Logger:    logger,
		Now:       time.Now,
		Sleep:     time.Sleep,
		Rand:      rand.Int64N,
		pushed:    map[string]bool{},
		emptyDays: map[string]bool{},
	}
}

// Window is one [Start, End) range and the row date it is pushed under.
type Window struct {
	Start, End time.Time
	DateKey    string
}

// Outcome is what happened to one window.
type Outcome struct {
	Window  Window
	Rows    int
	Empty   bool
	Skipped bool
	Err     error
}

// Failed reports whether the outcome should make a one-shot run exit non-zero: an error, or an
// empty window the source considers a fault.
func (o Outcome) Failed(s Source) bool {
	return o.Err != nil || (o.Empty && !s.EmptyIsHealthy())
}

// Run blocks, ticking immediately and then on the configured schedule (or every PushInterval
// when no schedule is set), until ctx is cancelled.
func (p *Pusher) Run(ctx context.Context) {
	p.tick(ctx)

	var ticker *time.Ticker
	if p.Config.Schedule == nil {
		ticker = time.NewTicker(p.Config.PushInterval)
		defer ticker.Stop()
	}

	for {
		var wait <-chan time.Time
		var timer *time.Timer
		if ticker != nil {
			wait = ticker.C
		} else {
			next := p.Config.Schedule.Next(p.Now(), p.Rand)
			p.Logger.Info("next scheduled push", "at", next)
			timer = time.NewTimer(next.Sub(p.Now()))
			wait = timer.C
		}

		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			p.Logger.Info("shutting down pusher loop", "reason", ctx.Err())
			return
		case <-wait:
			p.tick(ctx)
		}
	}
}

func (p *Pusher) tick(ctx context.Context) {
	windows := ClosedDayWindows(p.Now(), p.Config.CatchUpDays)
	p.runWindows(ctx, windows, false)
}

// RunOnce processes exactly the given windows and returns what happened to each, for the
// --once flag. Windows are always attempted: the already-delivered shortcut applies only to the
// long-running loop.
func (p *Pusher) RunOnce(ctx context.Context, windows []Window) []Outcome {
	return p.runWindows(ctx, windows, true)
}

func (p *Pusher) runWindows(ctx context.Context, windows []Window, force bool) []Outcome {
	outcomes := make([]Outcome, 0, len(windows))
	for i, w := range windows {
		if ctx.Err() != nil {
			break
		}
		newest := i == len(windows)-1
		outcomes = append(outcomes, p.processWindow(ctx, w, newest, force))
	}
	if len(windows) > 0 {
		p.Metrics.SetConsecutiveEmpty(p.consecutiveEmpty(windows[len(windows)-1].Start))
	}
	return outcomes
}

func (p *Pusher) processWindow(ctx context.Context, w Window, newest, force bool) Outcome {
	out := Outcome{Window: w}

	if !force && p.pushed[w.DateKey] {
		out.Skipped = true
		return out
	}

	start := p.Now()
	p.Logger.Info("starting push cycle", "source", p.Source.Name(), "date", w.DateKey, "window_start", w.Start, "window_end", w.End)

	payload, rowCount, err := p.Source.Collect(ctx, w.Start, w.End, w.DateKey)
	if err != nil {
		duration := p.Now().Sub(start)
		p.Metrics.RecordFailure(err, duration)
		p.Logger.Error("failed to query source", "source", p.Source.Name(), "date", w.DateKey, "error", err, "duration_seconds", duration.Seconds())
		out.Err = err
		return out
	}

	if rowCount == 0 {
		return p.handleEmpty(ctx, w, payload, newest, start, out)
	}

	delete(p.emptyDays, w.DateKey)
	payload = p.annotate(payload, w)

	p.Logger.Info("pushing batch", "source", p.Source.Name(), "date", w.DateKey, "row_count", rowCount)

	resp, err := p.pushWithRetry(ctx, payload)
	duration := p.Now().Sub(start)

	if err != nil {
		p.Metrics.RecordFailure(err, duration)
		p.Logger.Error("push failed after exhausting retries", "error", err, "date", w.DateKey, "duration_seconds", duration.Seconds(), "row_count", rowCount)
		out.Err = err
		return out
	}

	p.pushed[w.DateKey] = true
	out.Rows = rowCount
	p.Metrics.RecordSuccess(rowCount, duration)
	p.Metrics.SetLastSuccessWindow(w.End)
	p.Logger.Info("push succeeded", "source", p.Source.Name(), "date", w.DateKey, "accepted", resp.Accepted, "rejected", resp.Rejected, "duration_seconds", duration.Seconds())

	// Counts and provider ids only — nothing here is a credential or a prompt.
	if len(resp.HeldProviders) > 0 {
		p.Logger.Warn("spend for some providers is held awaiting a decision in Plutus (not counted yet)", "held_providers", resp.HeldProviders)
	}
	if a := resp.Activity; a != nil && (a.OverflowRows > 0 || a.InvalidRows > 0) {
		p.Logger.Warn("some activity labels were not used", "invalid_rows", a.InvalidRows, "overflow_rows", a.OverflowRows, "overflow_values", a.OverflowValues, "cap", a.Cap)
	}

	// A 2xx with rejected rows is a partial failure the customer needs to see — this is their
	// only visibility into it (design doc §1), so every error string is logged individually
	// rather than just the count.
	if resp.Rejected > 0 {
		p.Logger.Warn("some rows were rejected by Plutus", "rejected", resp.Rejected, "accepted", resp.Accepted)
		for _, e := range resp.Errors {
			p.Logger.Warn("row rejection detail", "error", e)
		}
	}
	return out
}

// handleEmpty deals with a window that produced no rows. Pushing an empty batch is not a way to
// say "no data": it is only done, for the newest window, to sources that implement Annotator,
// where it is a heartbeat the backend records without writing or deleting any cost rows.
func (p *Pusher) handleEmpty(ctx context.Context, w Window, payload any, newest bool, started time.Time, out Outcome) Outcome {
	out.Empty = true
	healthy := p.Source.EmptyIsHealthy()
	p.emptyDays[w.DateKey] = true
	p.Metrics.RecordEmpty(healthy, p.Now().Sub(started))

	consecutive := p.consecutiveEmpty(w.Start)
	switch {
	case healthy:
		p.Logger.Info("nothing to push for this window", "source", p.Source.Name(), "date", w.DateKey)
	case newest && consecutive >= 2:
		p.Logger.Error("source returned no rows for consecutive days", "source", p.Source.Name(), "date", w.DateKey, "consecutive_empty_days", consecutive)
	default:
		p.Logger.Warn("source returned no rows for this window", "source", p.Source.Name(), "date", w.DateKey)
	}

	if _, ok := p.Source.(Annotator); !ok || payload == nil || !newest {
		return out
	}

	payload = p.annotate(payload, w)
	if _, err := p.pushWithRetry(ctx, payload); err != nil {
		p.Metrics.RecordFailure(err, p.Now().Sub(started))
		p.Logger.Error("heartbeat push failed after exhausting retries", "date", w.DateKey, "error", err)
		out.Err = err
		return out
	}
	p.Logger.Info("heartbeat sent for an empty window", "source", p.Source.Name(), "date", w.DateKey)
	return out
}

func (p *Pusher) annotate(payload any, w Window) any {
	a, ok := p.Source.(Annotator)
	if !ok || payload == nil {
		return payload
	}
	return a.Annotate(payload, CollectorMeta{
		Version:        p.Config.Version,
		PrometheusMode: p.Config.PrometheusMode,
		WindowStart:    w.Start,
		WindowEnd:      w.End,
	})
}

// consecutiveEmpty counts empty days walking back from the day starting at newest. A day this
// process has no record of ends the run, so a restart does not invent a streak.
func (p *Pusher) consecutiveEmpty(newest time.Time) int {
	n := 0
	for d := newest.UTC(); p.emptyDays[d.Format("2006-01-02")]; d = d.AddDate(0, 0, -1) {
		n++
	}
	return n
}

// PreviousUTCDayWindow returns [start, end) for "yesterday" in UTC — the newest window every
// source in this repo is queried over. The UTC day is the unit because it is the only granularity
// that means the same thing across the systems Plutus reads (see plutus's CLAUDE.md on the cost
// horizon and redistribution), and because both OpenCost and LiteLLM aggregate daily anyway.
func PreviousUTCDayWindow(now time.Time) (start, end time.Time, dateKey string) {
	today := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day(), 0, 0, 0, 0, time.UTC)
	start = today.AddDate(0, 0, -1)
	end = today
	dateKey = start.Format("2006-01-02")
	return
}

// ClosedDayWindows returns the last n closed UTC days, oldest first. Pushing the days before
// yesterday as well is what lets a day that was missed or came back empty heal once its inputs
// are fixed: the backend replaces a day's rows per push, so re-sending a day is idempotent.
func ClosedDayWindows(now time.Time, n int) []Window {
	if n < 1 {
		n = 1
	}
	newestStart, _, _ := PreviousUTCDayWindow(now)
	windows := make([]Window, 0, n)
	for i := n - 1; i >= 0; i-- {
		start := newestStart.AddDate(0, 0, -i)
		windows = append(windows, Window{Start: start, End: start.AddDate(0, 0, 1), DateKey: start.Format("2006-01-02")})
	}
	return windows
}

// ParseWindow parses "<start>,<end>" (RFC3339 timestamps) for the --window flag. The rows are
// dated by the start's UTC day, and the backend replaces a whole day per push, so a window that
// is not a whole UTC day replaces that day with a partial one; it is meant for backfills of
// whole days and for end-to-end tests.
func ParseWindow(raw string) (Window, error) {
	parts := strings.Split(raw, ",")
	if len(parts) != 2 {
		return Window{}, fmt.Errorf("window must be <start>,<end> as RFC3339 timestamps, got %q", raw)
	}
	start, err := time.Parse(time.RFC3339, strings.TrimSpace(parts[0]))
	if err != nil {
		return Window{}, fmt.Errorf("window start: %w", err)
	}
	end, err := time.Parse(time.RFC3339, strings.TrimSpace(parts[1]))
	if err != nil {
		return Window{}, fmt.Errorf("window end: %w", err)
	}
	if !end.After(start) {
		return Window{}, errors.New("window end must be after its start")
	}
	start, end = start.UTC(), end.UTC()
	return Window{Start: start, End: end, DateKey: start.Format("2006-01-02")}, nil
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
