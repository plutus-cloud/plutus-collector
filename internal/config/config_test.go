package config

import "testing"

func fakeEnv(values map[string]string) func(string) string {
	return func(k string) string { return values[k] }
}

func TestLoadOpenCost_RequiresApiKeyClusterNameCurrency(t *testing.T) {
	_, err := LoadOpenCost(fakeEnv(map[string]string{}))
	if err == nil {
		t.Fatal("expected an error when required vars are missing")
	}
}

func TestLoadOpenCost_SucceedsWithRequiredVarsAndDefaults(t *testing.T) {
	cfg, err := LoadOpenCost(fakeEnv(map[string]string{
		"PLUTUS_API_KEY": "key123",
		"CLUSTER_NAME":   "prod-1",
		"CURRENCY":       "USD",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.OpenCostURL != defaultOpenCostURL {
		t.Errorf("expected default OpenCost URL, got %q", cfg.OpenCostURL)
	}
	if cfg.PlutusIngestURL != defaultIngestURL {
		t.Errorf("expected default ingest URL, got %q", cfg.PlutusIngestURL)
	}
	if cfg.PushInterval.Hours() != 24 {
		t.Errorf("expected default push interval of 24h, got %v", cfg.PushInterval)
	}
}

func TestLoadOpenCost_RejectsNonPositivePushInterval(t *testing.T) {
	_, err := LoadOpenCost(fakeEnv(map[string]string{
		"PLUTUS_API_KEY":        "key123",
		"CLUSTER_NAME":          "prod-1",
		"CURRENCY":              "USD",
		"PUSH_INTERVAL_MINUTES": "0",
	}))
	if err == nil {
		t.Fatal("expected an error for a non-positive push interval")
	}
}

func TestLoadOpenCost_AcceptsValidHttpsIngestURL(t *testing.T) {
	cfg, err := LoadOpenCost(fakeEnv(map[string]string{
		"PLUTUS_API_KEY":    "key123",
		"CLUSTER_NAME":      "prod-1",
		"CURRENCY":          "USD",
		"PLUTUS_INGEST_URL": "https://example.com/ingest",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.PlutusIngestURL != "https://example.com/ingest" {
		t.Errorf("expected ingest URL override, got %q", cfg.PlutusIngestURL)
	}
}

func TestLoadOpenCost_RejectsHttpIngestURL(t *testing.T) {
	_, err := LoadOpenCost(fakeEnv(map[string]string{
		"PLUTUS_API_KEY":    "key123",
		"CLUSTER_NAME":      "prod-1",
		"CURRENCY":          "USD",
		"PLUTUS_INGEST_URL": "http://example.com/ingest",
	}))
	if err == nil {
		t.Fatal("expected an error for a non-https PLUTUS_INGEST_URL (it carries a Bearer API key)")
	}
}

func TestLoadOpenCost_RejectsMalformedIngestURL(t *testing.T) {
	_, err := LoadOpenCost(fakeEnv(map[string]string{
		"PLUTUS_API_KEY":    "key123",
		"CLUSTER_NAME":      "prod-1",
		"CURRENCY":          "USD",
		"PLUTUS_INGEST_URL": "https:///no-host",
	}))
	if err == nil {
		t.Fatal("expected an error for a malformed PLUTUS_INGEST_URL with no host")
	}
}

func TestLoadOpenCost_RejectsMalformedOpenCostURL(t *testing.T) {
	_, err := LoadOpenCost(fakeEnv(map[string]string{
		"PLUTUS_API_KEY": "key123",
		"CLUSTER_NAME":   "prod-1",
		"CURRENCY":       "USD",
		"OPENCOST_URL":   "http:///no-host",
	}))
	if err == nil {
		t.Fatal("expected an error for a malformed OPENCOST_URL with no host")
	}
}

func TestLoadOpenCost_HonorsOverrides(t *testing.T) {
	cfg, err := LoadOpenCost(fakeEnv(map[string]string{
		"PLUTUS_API_KEY":        "key123",
		"CLUSTER_NAME":          "prod-1",
		"CURRENCY":              "EUR",
		"OPENCOST_URL":          "http://opencost.custom:9003",
		"PLUTUS_INGEST_URL":     "https://example.com/ingest",
		"PUSH_INTERVAL_MINUTES": "60",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Currency != "EUR" {
		t.Errorf("expected currency override EUR, got %q", cfg.Currency)
	}
	if cfg.OpenCostURL != "http://opencost.custom:9003" {
		t.Errorf("expected OpenCost URL override, got %q", cfg.OpenCostURL)
	}
	if cfg.PushInterval.Minutes() != 60 {
		t.Errorf("expected 60m push interval, got %v", cfg.PushInterval)
	}
}

// ─── LiteLLM ────────────────────────────────────────────────────────────────

func TestLoadLiteLLM_RequiresApiKeyBaseUrlAndMasterKey(t *testing.T) {
	_, err := LoadLiteLLM(fakeEnv(map[string]string{}))
	if err == nil {
		t.Fatal("expected an error when required vars are missing")
	}
	// One message listing every problem, not the first one — see the package comment. A
	// deployment missing three variables should learn that once, not across three restarts.
	for _, want := range []string{"PLUTUS_API_KEY", "LITELLM_BASE_URL", "LITELLM_MASTER_KEY"} {
		if !contains(err.Error(), want) {
			t.Errorf("expected the error to name %s, got: %v", want, err)
		}
	}
}

// The whole reason these are two loaders rather than one with a MODE switch: CLUSTER_NAME and
// CURRENCY are mandatory for OpenCost and meaningless here. Requiring them of a LiteLLM
// deployment would be asking for values nobody sets and nothing reads.
func TestLoadLiteLLM_DoesNotRequireClusterNameOrCurrency(t *testing.T) {
	cfg, err := LoadLiteLLM(fakeEnv(map[string]string{
		"PLUTUS_API_KEY":     "key123",
		"LITELLM_BASE_URL":   "http://litellm:4000",
		"LITELLM_MASTER_KEY": "sk-master",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.PlutusIngestURL != defaultLiteLLMIngestURL {
		t.Errorf("expected the litellm ingest default, got %q", cfg.PlutusIngestURL)
	}
	if cfg.PushInterval.Hours() != 24 {
		t.Errorf("expected the shared 24h default, got %v", cfg.PushInterval)
	}
}

// http:// is fine for the gateway (an internal address, never leaves the customer's network)
// and must never be fine for the ingest URL, which carries a Bearer key.
func TestLoadLiteLLM_AllowsPlainHttpGatewayButNotPlainHttpIngest(t *testing.T) {
	base := map[string]string{
		"PLUTUS_API_KEY":     "key123",
		"LITELLM_BASE_URL":   "http://litellm.internal:4000",
		"LITELLM_MASTER_KEY": "sk-master",
	}
	if _, err := LoadLiteLLM(fakeEnv(base)); err != nil {
		t.Fatalf("plain-http gateway URL should be accepted: %v", err)
	}

	base["PLUTUS_INGEST_URL"] = "http://console.plutus-cloud.com/api/ingest/litellm-cost"
	if _, err := LoadLiteLLM(fakeEnv(base)); err == nil {
		t.Fatal("expected plain-http ingest URL to be rejected — it carries a Bearer API key")
	}
}

func TestLoadLiteLLM_RejectsMalformedBaseUrl(t *testing.T) {
	_, err := LoadLiteLLM(fakeEnv(map[string]string{
		"PLUTUS_API_KEY":     "key123",
		"LITELLM_BASE_URL":   "not a url",
		"LITELLM_MASTER_KEY": "sk-master",
	}))
	if err == nil {
		t.Fatal("expected a malformed LITELLM_BASE_URL to be rejected")
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
