package pusher

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func fixedNow(t time.Time) func() time.Time { return func() time.Time { return t } }

func metricsText(t *testing.T, p *Pusher) string {
	t.Helper()
	rec := httptest.NewRecorder()
	p.Metrics.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	return rec.Body.String()
}

func readyzStatus(p *Pusher) int {
	rec := httptest.NewRecorder()
	p.Metrics.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/readyz", nil))
	return rec.Code
}

func TestScheduleNext(t *testing.T) {
	s := Schedule{Hour: 1, Minute: 30, Jitter: 20 * time.Minute}
	zero := func(n int64) int64 { return 0 }
	max := func(n int64) int64 { return n - 1 }

	cases := []struct {
		name string
		now  time.Time
		rnd  func(int64) int64
		want time.Time
	}{
		{"before today's slot", time.Date(2026, 10, 25, 0, 10, 0, 0, time.UTC), zero, time.Date(2026, 10, 25, 1, 30, 0, 0, time.UTC)},
		{"jitter pushes the slot later", time.Date(2026, 10, 25, 0, 10, 0, 0, time.UTC), max, time.Date(2026, 10, 25, 1, 50, 0, 0, time.UTC)},
		{"after today's slot rolls to tomorrow", time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC), zero, time.Date(2026, 10, 26, 1, 30, 0, 0, time.UTC)},
		{"exactly at the slot is not strictly after", time.Date(2026, 10, 25, 1, 30, 0, 0, time.UTC), zero, time.Date(2026, 10, 26, 1, 30, 0, 0, time.UTC)},
		{"month end", time.Date(2026, 10, 31, 23, 0, 0, 0, time.UTC), zero, time.Date(2026, 11, 1, 1, 30, 0, 0, time.UTC)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := s.Next(tc.now, tc.rnd); !got.Equal(tc.want) {
				t.Errorf("Next(%v) = %v, want %v", tc.now, got, tc.want)
			}
		})
	}
}

func TestClosedDayWindows_OldestFirstAndContiguous(t *testing.T) {
	ws := ClosedDayWindows(time.Date(2026, 10, 25, 1, 30, 0, 0, time.UTC), 3)
	if len(ws) != 3 {
		t.Fatalf("expected 3 windows, got %d", len(ws))
	}
	want := []string{"2026-10-22", "2026-10-23", "2026-10-24"}
	for i, w := range ws {
		if w.DateKey != want[i] {
			t.Errorf("window %d: date %s, want %s", i, w.DateKey, want[i])
		}
		if !w.End.Equal(w.Start.AddDate(0, 0, 1)) {
			t.Errorf("window %d is not a whole UTC day: %v to %v", i, w.Start, w.End)
		}
	}
	if got := len(ClosedDayWindows(time.Now(), 0)); got != 1 {
		t.Errorf("CatchUpDays below 1 should mean one window, got %d", got)
	}
}

func TestParseWindow(t *testing.T) {
	w, err := ParseWindow("2026-10-24T00:00:00Z,2026-10-25T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if w.DateKey != "2026-10-24" || !w.End.Equal(w.Start.Add(24*time.Hour)) {
		t.Errorf("unexpected window %+v", w)
	}
	for _, bad := range []string{"", "2026-10-24T00:00:00Z", "x,y", "2026-10-25T00:00:00Z,2026-10-24T00:00:00Z"} {
		if _, err := ParseWindow(bad); err == nil {
			t.Errorf("expected an error for %q", bad)
		}
	}
}

func TestTick_CatchUpPushesEachMissingDayOldestFirst(t *testing.T) {
	src := &fakeSource{perDate: map[string]int{"2026-10-22": 1, "2026-10-23": 1, "2026-10-24": 1}}
	fp := &fakePusher{}
	p := newTestPusher(src, fp)
	p.Config.CatchUpDays = 3
	p.Now = fixedNow(time.Date(2026, 10, 25, 1, 30, 0, 0, time.UTC))

	p.tick(context.Background())

	if got := strings.Join(src.collected, ","); got != "2026-10-22,2026-10-23,2026-10-24" {
		t.Errorf("collected %s", got)
	}
	if fp.calls != 3 {
		t.Errorf("expected 3 pushes, got %d", fp.calls)
	}
}

// A day this process already delivered is not queried again: re-sending it gains nothing, and if
// the metrics store has since lost history the second query could replace a complete day with a
// partial one.
func TestTick_AlreadyDeliveredDaysAreSkippedButFailedOnesRetry(t *testing.T) {
	src := &fakeSource{perDate: map[string]int{"2026-10-22": 1, "2026-10-23": 0, "2026-10-24": 1}}
	fp := &fakePusher{}
	p := newTestPusher(src, fp)
	p.Config.CatchUpDays = 3
	p.Now = fixedNow(time.Date(2026, 10, 25, 1, 30, 0, 0, time.UTC))
	p.tick(context.Background())

	src.collected = nil
	src.perDate["2026-10-23"] = 1 // the inputs were fixed
	p.Now = fixedNow(time.Date(2026, 10, 26, 1, 30, 0, 0, time.UTC))
	src.perDate["2026-10-25"] = 1
	p.tick(context.Background())

	if got := strings.Join(src.collected, ","); got != "2026-10-23,2026-10-25" {
		t.Errorf("second tick collected %s, want only the healed day and the new day", got)
	}
}

func TestTick_EmptyWindowFailsReadinessUnlessSourceAllowsIt(t *testing.T) {
	for _, tc := range []struct {
		name        string
		emptyOK     bool
		wantReady   int
		wantSuccess string
	}{
		{"fault", false, 503, "plutus_collector_last_push_success 0"},
		{"healthy", true, 200, "plutus_collector_last_push_success 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newTestPusher(&fakeSource{emptyOK: tc.emptyOK}, &fakePusher{})
			p.tick(context.Background())

			if got := readyzStatus(p); got != tc.wantReady {
				t.Errorf("/readyz = %d, want %d", got, tc.wantReady)
			}
			text := metricsText(t, p)
			if !strings.Contains(text, tc.wantSuccess) {
				t.Errorf("metrics missing %q:\n%s", tc.wantSuccess, text)
			}
			if !strings.Contains(text, "plutus_collector_empty_results_total 1") {
				t.Errorf("empty results counter not incremented:\n%s", text)
			}
		})
	}
}

func TestTick_RowsAfterAnEmptyWindowRestoreReadiness(t *testing.T) {
	src := &fakeSource{perDate: map[string]int{"2026-10-24": 0}}
	fp := &fakePusher{}
	p := newTestPusher(src, fp)
	p.Now = fixedNow(time.Date(2026, 10, 25, 1, 30, 0, 0, time.UTC))
	p.tick(context.Background())
	if readyzStatus(p) != 503 {
		t.Fatal("expected 503 after an empty window")
	}

	src.perDate["2026-10-24"] = 4
	p.tick(context.Background())
	if readyzStatus(p) != 200 {
		t.Error("expected readiness to recover once the window has rows")
	}
}

func TestTick_ConsecutiveEmptyDaysAreCounted(t *testing.T) {
	src := &fakeSource{}
	p := newTestPusher(src, &fakePusher{})
	p.Now = fixedNow(time.Date(2026, 10, 25, 1, 30, 0, 0, time.UTC))
	p.tick(context.Background())
	p.Now = fixedNow(time.Date(2026, 10, 26, 1, 30, 0, 0, time.UTC))
	p.tick(context.Background())

	if text := metricsText(t, p); !strings.Contains(text, "plutus_collector_consecutive_empty_windows 2") {
		t.Errorf("expected 2 consecutive empty windows:\n%s", text)
	}
}

// A heartbeat is sent for the newest empty window, only to a source that accepts a collector
// block, and carries the window and the agent's identity.
func TestTick_EmptyWindowSendsHeartbeatToAnnotatingSource(t *testing.T) {
	src := &annotatingSource{fakeSource{payload: testPayload{}}}
	fp := &fakePusher{}
	p := newTestPusher(src, fp)
	p.Config.Version = "v9.9.9"
	p.Config.PrometheusMode = "bundled"
	p.Now = fixedNow(time.Date(2026, 10, 25, 1, 30, 0, 0, time.UTC))

	p.tick(context.Background())

	if fp.calls != 1 {
		t.Fatalf("expected one heartbeat push, got %d", fp.calls)
	}
	a, ok := fp.lastPayload.(annotated)
	if !ok {
		t.Fatalf("heartbeat was not annotated: %T", fp.lastPayload)
	}
	if a.Meta.Version != "v9.9.9" || a.Meta.PrometheusMode != "bundled" {
		t.Errorf("unexpected meta %+v", a.Meta)
	}
	if !a.Meta.WindowStart.Equal(time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("wrong window start %v", a.Meta.WindowStart)
	}
}

func TestTick_OnlyNewestEmptyWindowGetsAHeartbeat(t *testing.T) {
	src := &annotatingSource{fakeSource{payload: testPayload{}}}
	fp := &fakePusher{}
	p := newTestPusher(src, fp)
	p.Config.CatchUpDays = 3
	p.Now = fixedNow(time.Date(2026, 10, 25, 1, 30, 0, 0, time.UTC))

	p.tick(context.Background())

	if fp.calls != 1 {
		t.Errorf("expected exactly one heartbeat for three empty days, got %d", fp.calls)
	}
}

func TestRunOnce_ReportsFailureForEmptyWindowOnlyWhereThatIsAFault(t *testing.T) {
	w, _ := ParseWindow("2026-10-24T00:00:00Z,2026-10-25T00:00:00Z")

	fault := newTestPusher(&fakeSource{}, &fakePusher{})
	out := fault.RunOnce(context.Background(), []Window{w})
	if len(out) != 1 || !out[0].Failed() {
		t.Errorf("an empty window should fail a one-shot run: %+v", out)
	}

	ok := newTestPusher(&fakeSource{emptyOK: true}, &fakePusher{})
	out = ok.RunOnce(context.Background(), []Window{w})
	if len(out) != 1 || out[0].Failed() {
		t.Errorf("an empty window is fine for a source that allows it: %+v", out)
	}
}

func TestRunOnce_AlwaysAttemptsEvenAnAlreadyDeliveredDay(t *testing.T) {
	src := &fakeSource{perDate: map[string]int{"2026-10-24": 2}}
	fp := &fakePusher{}
	p := newTestPusher(src, fp)
	w, _ := ParseWindow("2026-10-24T00:00:00Z,2026-10-25T00:00:00Z")

	p.RunOnce(context.Background(), []Window{w})
	p.RunOnce(context.Background(), []Window{w})

	if fp.calls != 2 {
		t.Errorf("expected both explicit runs to push, got %d", fp.calls)
	}
}
