package config

import "testing"

func openCostEnv(extra map[string]string) func(string) string {
	m := map[string]string{"PLUTUS_API_KEY": "key123", "CLUSTER_NAME": "prod-1", "CURRENCY": "USD"}
	for k, v := range extra {
		m[k] = v
	}
	return fakeEnv(m)
}

func TestLoadOpenCost_ScheduleCatchUpAndEmptyResultDefaults(t *testing.T) {
	cfg, err := LoadOpenCost(openCostEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ScheduleHour != 1 || cfg.ScheduleMinute != 30 || cfg.JitterMinutes != 20 {
		t.Errorf("unexpected schedule %d:%d +%dm", cfg.ScheduleHour, cfg.ScheduleMinute, cfg.JitterMinutes)
	}
	if cfg.CatchUpDays != 3 {
		t.Errorf("catch-up days = %d, want 3", cfg.CatchUpDays)
	}
	if cfg.EmptyResultIgnore {
		t.Error("an empty result must be a fault by default")
	}
	if cfg.PushIntervalSet {
		t.Error("PushIntervalSet must be false when PUSH_INTERVAL_MINUTES is unset")
	}
	if cfg.PrometheusMode != "external" {
		t.Errorf("prometheus mode = %q, want external", cfg.PrometheusMode)
	}
}

func TestLoadOpenCost_ExplicitPushIntervalIsRecorded(t *testing.T) {
	cfg, err := LoadOpenCost(openCostEnv(map[string]string{"PUSH_INTERVAL_MINUTES": "60"}))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.PushIntervalSet || cfg.PushInterval.Minutes() != 60 {
		t.Errorf("explicit interval not honoured: set=%v interval=%v", cfg.PushIntervalSet, cfg.PushInterval)
	}
}

func TestLoadOpenCost_ValidatesNewSettings(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"bad schedule":   {"PUSH_SCHEDULE_UTC": "25:99"},
		"bad jitter":     {"PUSH_JITTER_MINUTES": "-1"},
		"jitter too big": {"PUSH_JITTER_MINUTES": "999"},
		"zero catch-up":  {"CATCH_UP_DAYS": "0"},
		"huge catch-up":  {"CATCH_UP_DAYS": "400"},
		"bad empty":      {"EMPTY_RESULT": "maybe"},
		"bad prom mode":  {"PROMETHEUS_MODE": "thanos"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadOpenCost(openCostEnv(env)); err == nil {
				t.Error("expected a validation error")
			}
		})
	}
	cfg, err := LoadOpenCost(openCostEnv(map[string]string{
		"PUSH_SCHEDULE_UTC": "04:05", "CATCH_UP_DAYS": "5", "EMPTY_RESULT": "ignore", "PROMETHEUS_MODE": "bundled",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ScheduleHour != 4 || cfg.ScheduleMinute != 5 || cfg.CatchUpDays != 5 || !cfg.EmptyResultIgnore || cfg.PrometheusMode != "bundled" {
		t.Errorf("overrides not applied: %+v", cfg)
	}
}
