package bifrost

import (
	"encoding/json"
	"strconv"
	"strings"
)

// FieldMap names where each value this collector needs lives in a Bifrost log record. Each entry
// is an ordered list of candidate paths ("token_usage.prompt_tokens" descends into an object);
// the first path that yields a usable value wins.
//
// ─── WHY THIS IS CONFIGURABLE ───────────────────────────────────────────────
//
// These names were read from Bifrost's source (framework/logstore/tables.go's Log struct json
// tags and core/schemas' BifrostLLMUsage), NOT from a live response, and the token fields in
// particular are documented as having changed shape (flat columns are `json:"-"`, usage lives
// under `token_usage`). A schema difference on a customer's Bifrost version should be a one-line
// env override, not a code release. See README's "unverified" note.
type FieldMap struct {
	Timestamp    []string
	Status       []string
	Provider     []string
	Model        []string
	VirtualKey   []string
	Team         []string
	Cost         []string
	InputTokens  []string
	OutputTokens []string
}

// DefaultFields are the names taken from Bifrost's source. Names come before ids so a readable
// label wins; the id is a stable fallback, as the LiteLLM collector does with key hashes.
func DefaultFields() FieldMap {
	return FieldMap{
		Timestamp:    []string{"timestamp"},
		Status:       []string{"status"},
		Provider:     []string{"provider"},
		Model:        []string{"model"},
		VirtualKey:   []string{"virtual_key_name", "virtual_key_id"},
		Team:         []string{"team_name", "customer_name", "team_id", "customer_id"},
		Cost:         []string{"cost"},
		InputTokens:  []string{"token_usage.prompt_tokens", "token_usage.input_tokens", "prompt_tokens"},
		OutputTokens: []string{"token_usage.completion_tokens", "token_usage.output_tokens", "completion_tokens"},
	}
}

type rawRecord map[string]json.RawMessage

// lookup resolves a dotted path against a record without decoding the rest of it, so the
// (potentially large) prompt and response bodies in a log record are never parsed.
func (r rawRecord) lookup(path string) (json.RawMessage, bool) {
	parts := strings.Split(path, ".")
	cur := map[string]json.RawMessage(r)
	for i, p := range parts {
		v, ok := cur[p]
		if !ok || len(v) == 0 || string(v) == "null" {
			return nil, false
		}
		if i == len(parts)-1 {
			return v, true
		}
		var next map[string]json.RawMessage
		if err := json.Unmarshal(v, &next); err != nil {
			return nil, false
		}
		cur = next
	}
	return nil, false
}

func (r rawRecord) str(paths []string) string {
	for _, p := range paths {
		v, ok := r.lookup(p)
		if !ok {
			continue
		}
		var s string
		if json.Unmarshal(v, &s) == nil && strings.TrimSpace(s) != "" {
			return s
		}
		var n json.Number
		if json.Unmarshal(v, &n) == nil && n != "" {
			return n.String()
		}
	}
	return ""
}

// num returns the first parseable number among the paths, accepting a JSON number or a numeric
// string, and whether one was found. found distinguishes "cost 0" from "no cost field".
func (r rawRecord) num(paths []string) (float64, bool) {
	for _, p := range paths {
		v, ok := r.lookup(p)
		if !ok {
			continue
		}
		var f float64
		if json.Unmarshal(v, &f) == nil {
			return f, true
		}
		var s string
		if json.Unmarshal(v, &s) == nil {
			if f, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
				return f, true
			}
		}
	}
	return 0, false
}
