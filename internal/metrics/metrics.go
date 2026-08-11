// Package metrics exposes a tiny hand-rolled Prometheus text-format /metrics endpoint plus a
// /healthz liveness/readiness probe, so Alloy (already scraping everything else in the cluster
// per the parent repo's docs/monitoring.md) can pick this agent up the same way. No client
// library dependency — the metric set is small and fixed, and it keeps the binary dependency-free
// beyond the Go standard library.
package metrics

import (
	"fmt"
	"net/http"
	"sync"
	"time"
)

// State is the mutable, concurrency-safe last-push status the /metrics and /healthz handlers
// read from. pusher.Run updates it after every attempt (success or failure).
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

func (s *State) snapshot() (t time.Time, success, hadResult bool, rows int, dur time.Duration, attempts, failures int64, uptime time.Duration) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastPushTime, s.lastPushSuccess, s.lastPushHadResult, s.lastPushRows, s.lastPushDuration, s.pushAttemptsTotal, s.pushFailuresTotal, time.Since(s.startedAt)
}

// Handler returns an http.ServeMux exposing /metrics (Prometheus text exposition format) and
// /healthz (liveness/readiness — always 200 once the process is up; this agent has no
// dependency that should fail readiness, since a down OpenCost/Plutus endpoint is a push
// failure to retry, not a reason to stop serving traffic there is none of).
func (s *State) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		lastPushTime, success, hadResult, rows, dur, attempts, failures, uptime := s.snapshot()

		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

		fmt.Fprintln(w, "# HELP plutus_collector_uptime_seconds Seconds since the agent process started.")
		fmt.Fprintln(w, "# TYPE plutus_collector_uptime_seconds gauge")
		fmt.Fprintf(w, "plutus_collector_uptime_seconds %f\n", uptime.Seconds())

		fmt.Fprintln(w, "# HELP plutus_collector_push_attempts_total Total push attempts (one per tick, after retries are exhausted or it succeeds).")
		fmt.Fprintln(w, "# TYPE plutus_collector_push_attempts_total counter")
		fmt.Fprintf(w, "plutus_collector_push_attempts_total %d\n", attempts)

		fmt.Fprintln(w, "# HELP plutus_collector_push_failures_total Total pushes that failed after exhausting retries.")
		fmt.Fprintln(w, "# TYPE plutus_collector_push_failures_total counter")
		fmt.Fprintf(w, "plutus_collector_push_failures_total %d\n", failures)

		if hadResult {
			fmt.Fprintln(w, "# HELP plutus_collector_last_push_timestamp_seconds Unix timestamp of the last completed push attempt.")
			fmt.Fprintln(w, "# TYPE plutus_collector_last_push_timestamp_seconds gauge")
			fmt.Fprintf(w, "plutus_collector_last_push_timestamp_seconds %d\n", lastPushTime.Unix())

			fmt.Fprintln(w, "# HELP plutus_collector_last_push_success Whether the last push succeeded (1) or failed after retries (0).")
			fmt.Fprintln(w, "# TYPE plutus_collector_last_push_success gauge")
			fmt.Fprintf(w, "plutus_collector_last_push_success %d\n", boolToInt(success))

			fmt.Fprintln(w, "# HELP plutus_collector_last_push_row_count Number of allocation rows in the last successful push.")
			fmt.Fprintln(w, "# TYPE plutus_collector_last_push_row_count gauge")
			fmt.Fprintf(w, "plutus_collector_last_push_row_count %d\n", rows)

			fmt.Fprintln(w, "# HELP plutus_collector_last_push_duration_seconds Wall-clock duration of the last push attempt, including retries.")
			fmt.Fprintln(w, "# TYPE plutus_collector_last_push_duration_seconds gauge")
			fmt.Fprintf(w, "plutus_collector_last_push_duration_seconds %f\n", dur.Seconds())
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
