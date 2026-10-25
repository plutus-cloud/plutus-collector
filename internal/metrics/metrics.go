// Package metrics exposes a tiny hand-rolled Prometheus text-format /metrics endpoint plus the
// /healthz and /readyz probes, so Alloy (already scraping everything else in the cluster per the
// parent repo's docs/monitoring.md) can pick this agent up the same way. No client library
// dependency — the metric set is small and fixed, and it keeps the binary dependency-free beyond
// the Go standard library.
package metrics

import (
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"
)

// State is the mutable, concurrency-safe last-push status the /metrics, /healthz and /readyz
// handlers read from. pusher.Run updates it after every window it processes (success, failure
// or empty).
type State struct {
	mu sync.RWMutex

	lastPushTime      time.Time
	lastPushSuccess   bool
	lastPushHadResult bool
	lastPushRows      int
	lastPushDuration  time.Duration
	lastPushError     string

	pushAttemptsTotal int64
	pushFailuresTotal int64
	startedAt         time.Time

	emptyResultsTotal      int64
	consecutiveEmpty       int
	lastSuccessWindowEnd   time.Time
	notReadyEmptyWindow    bool
	preflight              map[string]string // check id -> "ok" | "warn" | "error"
	windowCoverageRatio    float64
	hasWindowCoverageRatio bool
}

func NewState() *State {
	return &State{startedAt: time.Now()}
}

// RecordSuccess is called after a batch was accepted (2xx), even if some rows were rejected —
// rejection detail is logged separately by the pusher; the metric here is about the push
// mechanism succeeding, matching "last push success/failure" from the design doc.
func (s *State) RecordSuccess(rows int, duration time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastPushTime = time.Now()
	s.lastPushSuccess = true
	s.lastPushHadResult = true
	s.lastPushRows = rows
	s.lastPushDuration = duration
	s.lastPushError = ""
	s.pushAttemptsTotal++
	s.notReadyEmptyWindow = false
}

// RecordFailure is called after every retry attempt is exhausted for a tick.
func (s *State) RecordFailure(err error, duration time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastPushTime = time.Now()
	s.lastPushSuccess = false
	s.lastPushHadResult = true
	s.lastPushDuration = duration
	if err != nil {
		s.lastPushError = err.Error()
	}
	s.pushAttemptsTotal++
	s.pushFailuresTotal++
}

// RecordEmpty is called when a window produced no rows. healthy is the source's own policy for
// that: a gateway with no traffic is fine (last_push_success stays 1 and readiness is
// unaffected), a cluster whose allocation engine returned nothing is not (last_push_success
// drops to 0 and /readyz returns 503 until a later window has rows).
func (s *State) RecordEmpty(healthy bool, duration time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastPushTime = time.Now()
	s.lastPushSuccess = healthy
	s.lastPushHadResult = true
	s.lastPushRows = 0
	s.lastPushDuration = duration
	s.lastPushError = ""
	s.pushAttemptsTotal++
	s.emptyResultsTotal++
	s.notReadyEmptyWindow = !healthy
}

// SetConsecutiveEmpty records how many of the most recent UTC days in a row came back empty.
func (s *State) SetConsecutiveEmpty(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.consecutiveEmpty = n
}

// SetLastSuccessWindow records the end of the newest window that was pushed with rows.
func (s *State) SetLastSuccessWindow(end time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if end.After(s.lastSuccessWindowEnd) {
		s.lastSuccessWindowEnd = end
	}
}

// SetPreflight replaces the latest preflight outcome, check id -> "ok" | "warn" | "error".
func (s *State) SetPreflight(results map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.preflight = make(map[string]string, len(results))
	for k, v := range results {
		s.preflight[k] = v
	}
}

// SetWindowCoverage records what fraction of the last requested window the metrics store held
// data for (0..1).
func (s *State) SetWindowCoverage(ratio float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.windowCoverageRatio = ratio
	s.hasWindowCoverageRatio = true
}

// readiness reports whether the agent should be marked ready, and why not. Readiness is about
// the data, not the process: liveness (/healthz) stays process-only so a data problem never gets
// the pod restarted, but `kubectl get pods` shows 0/1 so the problem is visible without reading
// logs.
func (s *State) readiness() (ready bool, reasons []string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.notReadyEmptyWindow {
		reasons = append(reasons, "the last window returned no cost rows")
	}
	ids := make([]string, 0, len(s.preflight))
	for id, sev := range s.preflight {
		if sev == "error" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		reasons = append(reasons, "preflight check failed: "+id)
	}
	return len(reasons) == 0, reasons
}

type snapshot struct {
	lastPushTime         time.Time
	success, hadResult   bool
	rows                 int
	dur                  time.Duration
	attempts, failures   int64
	uptime               time.Duration
	emptyTotal           int64
	consecutiveEmpty     int
	lastSuccessWindowEnd time.Time
	preflight            map[string]string
	coverage             float64
	hasCoverage          bool
}

func (s *State) snapshot() snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	pf := make(map[string]string, len(s.preflight))
	for k, v := range s.preflight {
		pf[k] = v
	}
	return snapshot{
		lastPushTime: s.lastPushTime, success: s.lastPushSuccess, hadResult: s.lastPushHadResult,
		rows: s.lastPushRows, dur: s.lastPushDuration, attempts: s.pushAttemptsTotal,
		failures: s.pushFailuresTotal, uptime: time.Since(s.startedAt),
		emptyTotal: s.emptyResultsTotal, consecutiveEmpty: s.consecutiveEmpty,
		lastSuccessWindowEnd: s.lastSuccessWindowEnd, preflight: pf,
		coverage: s.windowCoverageRatio, hasCoverage: s.hasWindowCoverageRatio,
	}
}

// Handler returns an http.ServeMux exposing /metrics (Prometheus text exposition format),
// /healthz (liveness — always 200 once the process is up; a down OpenCost or Plutus endpoint is
// a push failure to retry, not a reason to restart the container) and /readyz (200, or 503 with
// the reasons while the data is missing).
func (s *State) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		ready, reasons := s.readiness()
		if ready {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok\n"))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		for _, reason := range reasons {
			fmt.Fprintln(w, reason)
		}
	})

	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		m := s.snapshot()

		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

		fmt.Fprintln(w, "# HELP plutus_collector_uptime_seconds Seconds since the agent process started.")
		fmt.Fprintln(w, "# TYPE plutus_collector_uptime_seconds gauge")
		fmt.Fprintf(w, "plutus_collector_uptime_seconds %f\n", m.uptime.Seconds())

		fmt.Fprintln(w, "# HELP plutus_collector_push_attempts_total Total windows processed (one per UTC day per tick, after retries are exhausted or it succeeds).")
		fmt.Fprintln(w, "# TYPE plutus_collector_push_attempts_total counter")
		fmt.Fprintf(w, "plutus_collector_push_attempts_total %d\n", m.attempts)

		fmt.Fprintln(w, "# HELP plutus_collector_push_failures_total Total pushes that failed after exhausting retries.")
		fmt.Fprintln(w, "# TYPE plutus_collector_push_failures_total counter")
		fmt.Fprintf(w, "plutus_collector_push_failures_total %d\n", m.failures)

		fmt.Fprintln(w, "# HELP plutus_collector_empty_results_total Total windows for which the source returned no rows.")
		fmt.Fprintln(w, "# TYPE plutus_collector_empty_results_total counter")
		fmt.Fprintf(w, "plutus_collector_empty_results_total %d\n", m.emptyTotal)

		fmt.Fprintln(w, "# HELP plutus_collector_consecutive_empty_windows Most recent UTC days in a row that returned no rows.")
		fmt.Fprintln(w, "# TYPE plutus_collector_consecutive_empty_windows gauge")
		fmt.Fprintf(w, "plutus_collector_consecutive_empty_windows %d\n", m.consecutiveEmpty)

		if !m.lastSuccessWindowEnd.IsZero() {
			fmt.Fprintln(w, "# HELP plutus_collector_last_success_window_timestamp_seconds Unix timestamp of the end of the newest window pushed with rows.")
			fmt.Fprintln(w, "# TYPE plutus_collector_last_success_window_timestamp_seconds gauge")
			fmt.Fprintf(w, "plutus_collector_last_success_window_timestamp_seconds %d\n", m.lastSuccessWindowEnd.Unix())
		}

		if m.hasCoverage {
			fmt.Fprintln(w, "# HELP plutus_collector_window_coverage_ratio Fraction (0..1) of the last requested window the metrics store held data for.")
			fmt.Fprintln(w, "# TYPE plutus_collector_window_coverage_ratio gauge")
			fmt.Fprintf(w, "plutus_collector_window_coverage_ratio %f\n", m.coverage)
		}

		if len(m.preflight) > 0 {
			fmt.Fprintln(w, "# HELP plutus_collector_preflight_ok Whether a preflight check passed (1) or warned/failed (0).")
			fmt.Fprintln(w, "# TYPE plutus_collector_preflight_ok gauge")
			ids := make([]string, 0, len(m.preflight))
			for id := range m.preflight {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			for _, id := range ids {
				fmt.Fprintf(w, "plutus_collector_preflight_ok{check=%q} %d\n", id, boolToInt(m.preflight[id] == "ok"))
			}
		}

		if m.hadResult {
			fmt.Fprintln(w, "# HELP plutus_collector_last_push_timestamp_seconds Unix timestamp of the last completed push attempt.")
			fmt.Fprintln(w, "# TYPE plutus_collector_last_push_timestamp_seconds gauge")
			fmt.Fprintf(w, "plutus_collector_last_push_timestamp_seconds %d\n", m.lastPushTime.Unix())

			fmt.Fprintln(w, "# HELP plutus_collector_last_push_success Whether the last window succeeded (1) or failed after retries or came back empty where that is a fault (0).")
			fmt.Fprintln(w, "# TYPE plutus_collector_last_push_success gauge")
			fmt.Fprintf(w, "plutus_collector_last_push_success %d\n", boolToInt(m.success))

			fmt.Fprintln(w, "# HELP plutus_collector_last_push_row_count Number of allocation rows in the last window.")
			fmt.Fprintln(w, "# TYPE plutus_collector_last_push_row_count gauge")
			fmt.Fprintf(w, "plutus_collector_last_push_row_count %d\n", m.rows)

			fmt.Fprintln(w, "# HELP plutus_collector_last_push_duration_seconds Wall-clock duration of the last push attempt, including retries.")
			fmt.Fprintln(w, "# TYPE plutus_collector_last_push_duration_seconds gauge")
			fmt.Fprintf(w, "plutus_collector_last_push_duration_seconds %f\n", m.dur.Seconds())
		}
	})

	return mux
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
