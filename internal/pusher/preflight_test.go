package pusher

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/plutus-cloud/plutus-collector/internal/preflight"
)

type fakeChecker struct {
	report *preflight.Report
	calls  int
	oldest time.Time
}

func (f *fakeChecker) Run(ctx context.Context, oldestDayStart time.Time) *preflight.Report {
	f.calls++
	f.oldest = oldestDayStart
	return f.report
}

func report(results ...preflight.Result) *preflight.Report {
	return &preflight.Report{Results: results}
}

func res(id, sev string) preflight.Result {
	return preflight.Result{ID: id, Severity: sev, Message: id + " " + sev, Remedy: "fix " + id}
}

func TestTick_PreflightErrorsFailReadinessAndAreExported(t *testing.T) {
	p := newTestPusher(oneRowSource(), &fakePusher{})
	p.Preflight = &fakeChecker{report: report(res("opencost_series", preflight.Error), res("history", preflight.Warn))}

	p.tick(context.Background())

	if readyzStatus(p) != 503 {
		t.Error("an error-severity preflight check must fail readiness")
	}
	text := metricsText(t, p)
	if !strings.Contains(text, `plutus_collector_preflight_ok{check="opencost_series"} 0`) ||
		!strings.Contains(text, `plutus_collector_preflight_ok{check="history"} 0`) {
		t.Errorf("preflight metrics missing:\n%s", text)
	}
}

func TestTick_PreflightWarningsDoNotFailReadiness(t *testing.T) {
	p := newTestPusher(oneRowSource(), &fakePusher{})
	p.Preflight = &fakeChecker{report: report(res("usage_series", preflight.Warn))}
	p.tick(context.Background())
	if readyzStatus(p) != 200 {
		t.Error("warnings must not fail readiness")
	}
}

func TestTick_ABlockingCheckStopsThePushButStillSendsAHeartbeatNamingIt(t *testing.T) {
	src := &annotatingSource{fakeSource{perDate: map[string]int{"2026-10-24": 3}}}
	fp := &fakePusher{}
	p := newTestPusher(src, fp)
	p.Config.BlockOn = []string{"cluster_ambiguity"}
	p.Preflight = &fakeChecker{report: report(res("cluster_ambiguity", preflight.Error))}
	p.Now = fixedNow(time.Date(2026, 10, 25, 1, 30, 0, 0, time.UTC))

	p.tick(context.Background())

	if len(src.collected) != 0 {
		t.Errorf("a blocked run must not query the source, queried %v", src.collected)
	}
	if fp.calls != 1 {
		t.Fatalf("expected exactly the heartbeat, got %d pushes", fp.calls)
	}
	hb, ok := fp.lastPayload.(annotated)
	if !ok || hb.Meta.EmptyReason != "preflight_blocked:cluster_ambiguity" {
		t.Errorf("heartbeat should name the blocking check: %+v", fp.lastPayload)
	}
	if hb.Meta.Preflight["cluster_ambiguity"] != "error" {
		t.Errorf("heartbeat should carry the preflight summary: %+v", hb.Meta.Preflight)
	}
}

func TestTick_ANonBlockingErrorStillPushes(t *testing.T) {
	fp := &fakePusher{}
	p := newTestPusher(oneRowSource(), fp)
	p.Config.BlockOn = []string{"cluster_ambiguity"}
	p.Preflight = &fakeChecker{report: report(res("usage_series", preflight.Error))}
	p.tick(context.Background())
	if fp.calls != 1 {
		t.Errorf("only listed checks block a push, got %d pushes", fp.calls)
	}
}

func TestTick_PreflightSummaryRidesOnAnOrdinaryPush(t *testing.T) {
	src := &annotatingSource{fakeSource{perDate: map[string]int{"2026-10-24": 2}}}
	fp := &fakePusher{}
	p := newTestPusher(src, fp)
	p.Preflight = &fakeChecker{report: report(res("history", preflight.Warn))}
	p.Now = fixedNow(time.Date(2026, 10, 25, 1, 30, 0, 0, time.UTC))

	p.tick(context.Background())

	a := fp.lastPayload.(annotated)
	if a.Meta.Preflight["history"] != "warn" || a.Meta.EmptyReason != "" {
		t.Errorf("unexpected meta %+v", a.Meta)
	}
}

// The first window after an install predates the store's data. That is not a fault: readiness
// stays up, the push is healthy, and the heartbeat says why there is nothing.
func TestTick_AWindowBeforeTheStoresHistoryIsNotAFault(t *testing.T) {
	src := &annotatingSource{fakeSource{payload: testPayload{}}}
	fp := &fakePusher{}
	p := newTestPusher(src, fp)
	p.Now = fixedNow(time.Date(2026, 10, 25, 1, 30, 0, 0, time.UTC))
	rep := report(res("history", preflight.Warn))
	rep.OldestSample = time.Date(2026, 10, 24, 15, 0, 0, 0, time.UTC) // after the newest window ended? no: window is 10-24 00:00-10-25 00:00
	rep.OldestSample = time.Date(2026, 10, 25, 0, 30, 0, 0, time.UTC) // OpenCost started after the window closed
	p.Preflight = &fakeChecker{report: rep}

	p.tick(context.Background())

	if readyzStatus(p) != 200 {
		t.Error("a fresh install must not report unready for a window with no history")
	}
	hb, ok := fp.lastPayload.(annotated)
	if !ok || hb.Meta.EmptyReason != "no_history" {
		t.Errorf("heartbeat should say no_history: %+v", fp.lastPayload)
	}
	if text := metricsText(t, p); !strings.Contains(text, "plutus_collector_last_push_success 1") {
		t.Errorf("a no-history window is not a failed push:\n%s", text)
	}
}

// With history that covers the window, an empty result is still a fault.
func TestTick_AnEmptyWindowInsideTheStoresHistoryStaysAFault(t *testing.T) {
	src := &annotatingSource{fakeSource{payload: testPayload{}}}
	p := newTestPusher(src, &fakePusher{})
	p.Now = fixedNow(time.Date(2026, 10, 25, 1, 30, 0, 0, time.UTC))
	rep := report()
	rep.OldestSample = time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	p.Preflight = &fakeChecker{report: rep}

	p.tick(context.Background())

	if readyzStatus(p) != 503 {
		t.Error("expected unready: the store has data for the window but OpenCost returned none")
	}
}

func TestTick_CatchUpDaysBeforeTheStoresHistoryAreNotQueried(t *testing.T) {
	src := &fakeSource{perDate: map[string]int{"2026-10-22": 1, "2026-10-23": 1, "2026-10-24": 1}}
	p := newTestPusher(src, &fakePusher{})
	p.Config.CatchUpDays = 3
	p.Now = fixedNow(time.Date(2026, 10, 25, 1, 30, 0, 0, time.UTC))
	rep := report()
	rep.OldestSample = time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	p.Preflight = &fakeChecker{report: rep}

	p.tick(context.Background())

	if got := strings.Join(src.collected, ","); got != "2026-10-23,2026-10-24" {
		t.Errorf("collected %s; the day that ended before the first sample has nothing to return", got)
	}
}

func TestTick_WindowCoverageIsExported(t *testing.T) {
	p := newTestPusher(oneRowSource(), &fakePusher{})
	p.Now = fixedNow(time.Date(2026, 10, 25, 1, 30, 0, 0, time.UTC))
	rep := report()
	rep.OldestSample = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC) // half of 10-24 covered
	p.Preflight = &fakeChecker{report: rep}

	p.tick(context.Background())

	if text := metricsText(t, p); !strings.Contains(text, "plutus_collector_window_coverage_ratio 0.5") {
		t.Errorf("expected a coverage ratio of 0.5:\n%s", text)
	}
}

func TestRefreshPreflight_ReportsRecoveryOnlyOnTheTransition(t *testing.T) {
	chk := &fakeChecker{report: report(res("opencost_series", preflight.Error))}
	p := newTestPusher(oneRowSource(), &fakePusher{})
	p.Preflight = chk

	if p.refreshPreflight(context.Background()) {
		t.Error("a failing run is not a recovery")
	}
	chk.report = report(res("opencost_series", preflight.OK))
	if !p.refreshPreflight(context.Background()) {
		t.Error("error then clean should report recovery")
	}
	if p.refreshPreflight(context.Background()) {
		t.Error("clean then clean is not a recovery")
	}
	if readyzStatus(p) != 200 {
		t.Error("readiness should follow the fix without waiting for the next push")
	}
}

func TestRunOnce_RunsPreflightAndHonoursBlocking(t *testing.T) {
	src := &annotatingSource{fakeSource{perDate: map[string]int{"2026-10-24": 3}}}
	p := newTestPusher(src, &fakePusher{})
	p.Config.BlockOn = []string{"cluster_ambiguity"}
	p.Preflight = &fakeChecker{report: report(res("cluster_ambiguity", preflight.Error))}
	w, _ := ParseWindow("2026-10-24T00:00:00Z,2026-10-25T00:00:00Z")

	out := p.RunOnce(context.Background(), []Window{w})

	if len(out) != 1 || !out[0].Failed() {
		t.Errorf("a blocked one-shot run must fail: %+v", out)
	}
}
