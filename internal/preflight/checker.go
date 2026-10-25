package preflight

import (
	"context"
	"fmt"
	"io"
	"time"
)

// Checker re-runs the same checks with a per-run oldest-day start, which is the one input that
// changes between a push and the next. It is what the pusher holds.
type Checker struct {
	Base Config
}

// Run executes the checks. oldestDayStart is the start of the oldest day the pusher is about to
// send.
func (c *Checker) Run(ctx context.Context, oldestDayStart time.Time) *Report {
	cfg := c.Base
	cfg.OldestDayStart = oldestDayStart
	return Run(ctx, cfg)
}

// Print writes a report for a person: one line per check, then the remedy under any that did not
// pass. It is the output of the `preflight` subcommand and of `helm test`.
func Print(w io.Writer, rep *Report) {
	for _, r := range rep.Results {
		fmt.Fprintf(w, "%-5s %-22s %s\n", mark(r.Severity), r.ID, r.Message)
		if r.Severity != OK && r.Remedy != "" {
			fmt.Fprintf(w, "      %-22s fix: %s\n", "", r.Remedy)
		}
	}
}

func mark(severity string) string { return severity }
