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
)

// testPayload stands in for whatever a real source produces. The pusher's subject is the retry
// and failure policy, not the data — these tests were written against a Kubernetes batch and are
// deliberately source-agnostic now, so they keep testing the policy when a third source arrives.
type testPayload struct {
	Rows int
}

type fakeSource struct {
	payload  any
	rowCount int
	err      error
}

func (f *fakeSource) Name() string { return "FakeSource" }

func (f *fakeSource) Collect(ctx context.Context, start, end time.Time, dateKey string) (any, int, error) {
	return f.payload, f.rowCount, f.err
}

func oneRowSource() *fakeSource {
	return &fakeSource{payload: testPayload{Rows: 1}, rowCount: 1}
}

// fakePusher fails with a RetryableError `failUntil` times before succeeding, so tests can
// assert the retry/backoff loop actually retries and eventually gives up or succeeds.
type fakePusher struct {
	failUntil   int
	calls       int
	lastPayload any
}

func (f *fakePusher) Push(ctx context.Context, payload any) (*ingest.Response, error) {
	f.calls++
	f.lastPayload = payload
	if f.calls <= f.failUntil {
		return nil, &ingest.RetryableError{Err: errors.New("simulated network failure")}
	}
	return &ingest.Response{Accepted: 1}, nil
}

func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestPusher(source Source, ip BatchPusher) *Pusher {
	p := New(source, ip, metrics.NewState(), Config{
		PushInterval:   time.Hour,
		MaxRetries:     3,
		RetryBaseDelay: time.Millisecond, // fast in tests
	}, silentLogger())
	p.Sleep = func(time.Duration) {} // no real sleeping in tests
	return p
}

func TestTick_SucceedsOnFirstAttempt(t *testing.T) {
	fp := &fakePusher{failUntil: 0}
	p := newTestPusher(oneRowSource(), fp)

	p.tick(context.Background())

	if fp.calls != 1 {
		t.Errorf("expected exactly 1 push call, got %d", fp.calls)
	}
	if fp.lastPayload != (testPayload{Rows: 1}) {
		t.Errorf("pusher altered the source's payload: %+v", fp.lastPayload)
	}
}

func TestTick_RetriesOnFailureThenSucceeds(t *testing.T) {
	fp := &fakePusher{failUntil: 2} // fails twice, succeeds on 3rd call
	p := newTestPusher(oneRowSource(), fp)

	p.tick(context.Background())

	if fp.calls != 3 {
		t.Errorf("expected 3 push calls (2 failures + 1 success), got %d", fp.calls)
	}
}

func TestTick_GivesUpAfterMaxRetriesAndDoesNotPanic(t *testing.T) {
	fp := &fakePusher{failUntil: 999} // always fails
	p := newTestPusher(oneRowSource(), fp)

	p.tick(context.Background()) // must not panic or crash the process

	// MaxRetries=3 means attempt 0 (first try) + 3 retries = 4 total calls.
	if fp.calls != 4 {
		t.Errorf("expected 4 push calls (1 + 3 retries), got %d", fp.calls)
	}
}

func TestTick_SourceFailureDoesNotPanic(t *testing.T) {
	fp := &fakePusher{}
	p := newTestPusher(&fakeSource{err: errors.New("source unreachable")}, fp)

	p.tick(context.Background()) // must not panic

	if fp.calls != 0 {
		t.Errorf("expected no push attempt when the source query fails, got %d calls", fp.calls)
	}
}

// An empty result is a real, non-error outcome — a gateway with no traffic yesterday, or a
// cluster whose rows were all zero-cost. It must not become an empty push: the backend's ingest
// routes delete the day's window before inserting, so pushing nothing would turn "no new data"
// into "delete what you already have".
func TestTick_EmptyResultIsNotPushed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		source *fakeSource
	}{
		{"nil payload", &fakeSource{payload: nil, rowCount: 0}},
		{"zero rows", &fakeSource{payload: testPayload{}, rowCount: 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fp := &fakePusher{}
			p := newTestPusher(tc.source, fp)

			p.tick(context.Background())

			if fp.calls != 0 {
				t.Errorf("expected no push for an empty result, got %d calls", fp.calls)
			}
		})
	}
}

func TestTick_NonRetryableErrorStopsImmediately(t *testing.T) {
	np := &nonRetryablePusher{}
	p := newTestPusher(oneRowSource(), np)

	p.tick(context.Background())

	if np.calls != 1 {
		t.Errorf("expected exactly 1 attempt for a non-retryable error, got %d", np.calls)
	}
}

func TestPushWithRetry_CancelledContextInterruptsBackoffSleepPromptly(t *testing.T) {
	fp := &fakePusher{failUntil: 999} // always fails, so a retry/backoff is scheduled
	p := newTestPusher(&fakeSource{}, fp)
	p.Config.RetryBaseDelay = time.Hour // would block far longer than the test timeout if not interrupted
	p.Sleep = time.Sleep                // use the real, blocking Sleep to prove interruption works

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		_, err := p.pushWithRetry(ctx, testPayload{})
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

func (n *nonRetryablePusher) Push(ctx context.Context, payload any) (*ingest.Response, error) {
	n.calls++
	return nil, errors.New("401 unauthorized: bad api key")
}
