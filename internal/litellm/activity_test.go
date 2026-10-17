package litellm

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Fixtures cover the three shapes request tags can arrive in. LiteLLM's own source types
// request_tags as a list in memory and as a JSON string on the stored row; the docs describe
// tags being sent as metadata.tags.
const activityFixtures = `[
 {"spend": 1.0, "model_group": "m", "custom_llm_provider": "openai", "api_key": "k", "team_id": "t", "prompt_tokens": 10, "completion_tokens": 1, "request_tags": ["activity:code-review", "User-Agent: curl"]},
 {"spend": 2.0, "model_group": "m", "custom_llm_provider": "openai", "api_key": "k", "team_id": "t", "prompt_tokens": 20, "completion_tokens": 2, "request_tags": "[\"activity:code-review\"]"},
 {"spend": 4.0, "model_group": "m", "custom_llm_provider": "openai", "api_key": "k", "team_id": "t", "prompt_tokens": 40, "completion_tokens": 4, "metadata": {"tags": ["activity:triage"]}},
 {"spend": 8.0, "model_group": "m", "custom_llm_provider": "openai", "api_key": "k", "team_id": "t", "prompt_tokens": 80, "completion_tokens": 8, "request_tags": ["activity:a", "activity:b"]},
 {"spend": 16.0, "model_group": "m", "custom_llm_provider": "openai", "api_key": "k", "team_id": "t", "prompt_tokens": 160, "completion_tokens": 16, "request_tags": ["unrelated"]},
 {"spend": 32.0, "model_group": "m", "custom_llm_provider": "openai", "api_key": "k", "team_id": "t", "prompt_tokens": 320, "completion_tokens": 32, "request_tags": null}
]`

func fixtureEntries(t *testing.T) []LogEntry {
	t.Helper()
	var es []LogEntry
	if err := json.Unmarshal([]byte(activityFixtures), &es); err != nil {
		t.Fatalf("fixture did not decode: %v", err)
	}
	return es
}

func TestActivityFor(t *testing.T) {
	cases := []struct {
		name string
		tags []string
		want string
	}{
		{"one match", []string{"x", "activity:review"}, "review"},
		{"zero matches", []string{"x"}, ""},
		{"two distinct matches omit", []string{"activity:a", "activity:b"}, ""},
		{"duplicate identical tag is one tag", []string{"activity:a", "activity:a"}, "a"},
		{"empty remainder is not a label", []string{"activity:"}, ""},
		{"no tags", nil, ""},
	}
	for _, c := range cases {
		if got := (LogEntry{RequestTags: c.tags}).ActivityFor("activity:"); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
	if got := (LogEntry{RequestTags: []string{"activity:a"}}).ActivityFor(""); got != "" {
		t.Errorf("empty prefix must disable the feature, got %q", got)
	}
	if got := (LogEntry{RequestTags: []string{"team=ml"}}).ActivityFor("team="); got != "ml" {
		t.Errorf("custom prefix should work, got %q", got)
	}
}

func TestTagListDecodesListStringAndGarbage(t *testing.T) {
	es := fixtureEntries(t)
	if len(es[0].RequestTags) != 2 || len(es[1].RequestTags) != 1 {
		t.Errorf("list and JSON-string forms should both decode: %v / %v", es[0].RequestTags, es[1].RequestTags)
	}
	var e LogEntry
	if err := json.Unmarshal([]byte(`{"spend":1,"request_tags":42}`), &e); err != nil || len(e.RequestTags) != 0 {
		t.Errorf("garbage tags must degrade to none without failing the parse: %v %v", err, e.RequestTags)
	}
}

func TestAggregateWithActivity_SplitsRowsByActivityAndKeepsTotals(t *testing.T) {
	es := fixtureEntries(t)
	rows := AggregateWithActivity("2026-06-01", es, "activity:")
	got := map[string]Row{}
	var total float64
	for _, r := range rows {
		got[r.Activity] = r
		total += r.Spend
	}
	if len(rows) != 3 {
		t.Fatalf("want 3 rows (code-review, triage, unlabelled), got %d: %+v", len(rows), rows)
	}
	if r := got["code-review"]; r.Spend != 3 || r.InputTokens != 30 || r.OutputTokens != 3 {
		t.Errorf("code-review should sum the list and JSON-string entries: %+v", r)
	}
	if got["triage"].Spend != 4 {
		t.Errorf("metadata.tags fallback failed: %+v", got["triage"])
	}
	// Two activity tags (8), an unrelated tag (16) and null tags (32) all fall to unlabelled.
	if got[""].Spend != 56 {
		t.Errorf("ambiguous/untagged spend must stay unlabelled, got %+v", got[""])
	}
	if total != 63 {
		t.Errorf("activity must not change the total, got %v", total)
	}
}

// The opt-in guarantee: with the flag off, the wire bytes equal what a collector without this
// feature would send.
func TestAggregate_WithoutPrefixIsByteIdentical(t *testing.T) {
	es := fixtureEntries(t)
	off, _ := json.Marshal(Batch{Rows: Aggregate("2026-06-01", es)})
	if strings.Contains(string(off), "activity") {
		t.Fatalf("activity must not appear when the feature is off: %s", off)
	}
	if len(Aggregate("2026-06-01", es)) != 1 {
		t.Errorf("without the prefix all entries share one tuple")
	}
	on, _ := json.Marshal(Batch{Rows: AggregateWithActivity("2026-06-01", es, "activity:")})
	if !strings.Contains(string(on), `"activity":"code-review"`) {
		t.Errorf("expected activity on the wire when enabled: %s", on)
	}
	// Unlabelled rows omit the key rather than sending "".
	if strings.Contains(string(on), `"activity":""`) {
		t.Errorf("unlabelled rows must omit activity: %s", on)
	}
}

func TestSource_PassesPrefixThrough(t *testing.T) {
	s := &Source{Logs: &fakeLogs{entries: fixtureEntries(t)}, ActivityTagPrefix: "activity:"}
	payload, n, err := s.Collect(context.Background(), time.Now(), time.Now(), "2026-06-01")
	if err != nil || n != 3 {
		t.Fatalf("want 3 rows, got %d err=%v payload=%v", n, err, payload)
	}
}
