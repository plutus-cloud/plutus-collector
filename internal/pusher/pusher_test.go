package pusher

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/plutus-cloud/plutus-collector/internal/ingest"
	"github.com/plutus-cloud/plutus-collector/internal/metrics"
	"github.com/plutus-cloud/plutus-collector/internal/opencost"
)

type fakeSource struct {
	rows []opencost.Row
	err  error
}

func (f *fakeSource) ComputeAllocation(ctx context.Context, start, end time.Time) ([]opencost.Row, error) {
	return f.rows, f.err
}

// fakePusher fails with a RetryableError `failUntil` times before succeeding, so tests can
// assert the retry/backoff loop actually retries and eventually gives up or succeeds.
type fakePusher struct {
	failUntil int
	calls     int
	lastBatch ingest.Batch
}

func (f *fakePusher) Push(ctx context.Context, batch ingest.Batch) (*ingest.Response, error) {
	f.calls++
	f.lastBatch = batch
	if f.calls <= f.failUntil {
		return nil, &ingest.RetryableError{Err: errors.New("simulated network failure")}
	}
	return &ingest.Response{Accepted: len(batch.Rows)}, nil
}

func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestPusher(source AllocationSource, ip BatchPusher) *Pusher {
	p := New(source, ip, metrics.NewState(), Config{
		ClusterName:    "test-cluster",
		Currency:       "USD",
		PushInterval:   time.Hour,
		MaxRetries:     3,
		RetryBaseDelay: time.Millisecond, // fast in tests
	}, silentLogger())
	p.Sleep = func(time.Duration) {} // no real sleeping in tests
	return p
}

func TestTick_SucceedsOnFirstAttempt(t *testing.T) {
	source := &fakeSource{rows: []opencost.Row{
		{Namespace: "checkout", Cluster: "prod-1", TotalCost: 5.0, HasTotalCost: true},
	}}
	fp := &fakePusher{failUntil: 0}
	p := newTestPusher(source, fp)

	p.tick(context.Background())

	if fp.calls != 1 {
		t.Errorf("expected exactly 1 push call, got %d", fp.calls)
	}
	if len(fp.lastBatch.Rows) != 1 {
		t.Errorf("expected 1 row in pushed batch, got %d", len(fp.lastBatch.Rows))
	}
	if fp.lastBatch.Currency != "USD" || fp.lastBatch.ClusterName != "test-cluster" {
		t.Errorf("unexpected batch header: %+v", fp.lastBatch)
	}
}

func TestTick_RetriesOnFailureThenSucceeds(t *testing.T) {
	source := &fakeSource{rows: []opencost.Row{
		{Namespace: "checkout", Cluster: "prod-1", TotalCost: 5.0, HasTotalCost: true},
	}}
	fp := &fakePusher{failUntil: 2} // fails twice, succeeds on 3rd call
	p := newTestPusher(source, fp)

	p.tick(context.Background())

	if fp.calls != 3 {
		t.Errorf("expected 3 push calls (2 failures + 1 success), got %d", fp.calls)
	}
}

func TestTick_GivesUpAfterMaxRetriesAndDoesNotPanic(t *testing.T) {
	source := &fakeSource{rows: []opencost.Row{
		{Namespace: "checkout", Cluster: "prod-1", TotalCost: 5.0, HasTotalCost: true},
	}}
	fp := &fakePusher{failUntil: 999} // always fails
	p := newTestPusher(source, fp)

	p.tick(context.Background()) // must not panic or crash the process

	// MaxRetries=3 means attempt 0 (first try) + 3 retries = 4 total calls.
	if fp.calls != 4 {
		t.Errorf("expected 4 push calls (1 + 3 retries), got %d", fp.calls)
	}
}

func TestTick_OpenCostFailureDoesNotPanic(t *testing.T) {
	source := &fakeSource{err: errors.New("opencost unreachable")}
	fp := &fakePusher{}
	p := newTestPusher(source, fp)

	p.tick(context.Background()) // must not panic

	if fp.calls != 0 {
		t.Errorf("expected no push attempt when OpenCost query fails, got %d calls", fp.calls)
	}
}

func TestTick_NonRetryableErrorStopsImmediately(t *testing.T) {
	source := &fakeSource{rows: []opencost.Row{
		{Namespace: "checkout", Cluster: "prod-1", TotalCost: 5.0, HasTotalCost: true},
	}}
	np := &nonRetryablePusher{}
	p := newTestPusher(source, np)

	p.tick(context.Background())

	if np.calls != 1 {
		t.Errorf("expected exactly 1 attempt for a non-retryable error, got %d", np.calls)
	}
}

func TestPushWithRetry_CancelledContextInterruptsBackoffSleepPromptly(t *testing.T) {
	source := &fakeSource{}
	fp := &fakePusher{failUntil: 999} // always fails, so a retry/backoff is scheduled
	p := newTestPusher(source, fp)
	p.Config.RetryBaseDelay = time.Hour // would block far longer than the test timeout if not interrupted
	p.Sleep = time.Sleep                // use the real, blocking Sleep to prove interruption works

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		_, err := p.pushWithRetry(ctx, ingest.Batch{})
		if !errors.Is(err, context.Canceled) {
			t.Errorf("expected context.Canceled, got %v", err)
		}
		close(done)
	}()

	// Give pushWithRetry a moment to make its first (failing) attempt and enter the backoff
	// sleep, then cancel — it must return promptly rather than waiting out the 1h delay.
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pushWithRetry did not return promptly after context cancellation")
	}
}

type nonRetryablePusher struct{ calls int }

func (n *nonRetryablePusher) Push(ctx context.Context, batch ingest.Batch) (*ingest.Response, error) {
	n.calls++
	return nil, errors.New("401 unauthorized: bad api key")
}
