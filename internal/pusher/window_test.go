package pusher

import (
	"testing"
	"time"
)

func TestPreviousUTCDayWindow(t *testing.T) {
	now := time.Date(2026, 8, 10, 15, 30, 0, 0, time.UTC)
	start, end, dateKey := PreviousUTCDayWindow(now)

	wantStart := time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC)
	wantEnd := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)

	if !start.Equal(wantStart) {
		t.Errorf("expected start %v, got %v", wantStart, start)
	}
	if !end.Equal(wantEnd) {
		t.Errorf("expected end %v, got %v", wantEnd, end)
	}
	if dateKey != "2026-08-09" {
		t.Errorf("expected dateKey 2026-08-09, got %q", dateKey)
	}
}
