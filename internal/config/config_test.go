package config

import "testing"

func fakeEnv(values map[string]string) func(string) string {
	return func(k string) string { return values[k] }
}

func TestLoad_RequiresApiKeyClusterNameCurrency(t *testing.T) {
	_, err := Load(fakeEnv(map[string]string{}))
	if err == nil {
		t.Fatal("expected an error when required vars are missing")
	}
}

func TestLoad_SucceedsWithRequiredVarsAndDefaults(t *testing.T) {
	cfg, err := Load(fakeEnv(map[string]string{
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

func TestLoad_RejectsNonPositivePushInterval(t *testing.T) {
	_, err := Load(fakeEnv(map[string]string{
		"PLUTUS_API_KEY":        "key123",
		"CLUSTER_NAME":          "prod-1",
		"CURRENCY":              "USD",
		"PUSH_INTERVAL_MINUTES": "0",
	}))
	if err == nil {
		t.Fatal("expected an error for a non-positive push interval")
	}
}

func TestLoad_AcceptsValidHttpsIngestURL(t *testing.T) {
	cfg, err := Load(fakeEnv(map[string]string{
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

func TestLoad_RejectsHttpIngestURL(t *testing.T) {
	_, err := Load(fakeEnv(map[string]string{
		"PLUTUS_API_KEY":    "key123",
		"CLUSTER_NAME":      "prod-1",
		"CURRENCY":          "USD",
		"PLUTUS_INGEST_URL": "http://example.com/ingest",
	}))
	if err == nil {
		t.Fatal("expected an error for a non-https PLUTUS_INGEST_URL (it carries a Bearer API key)")
	}
}

func TestLoad_RejectsMalformedIngestURL(t *testing.T) {
	_, err := Load(fakeEnv(map[string]string{
		"PLUTUS_API_KEY":    "key123",
		"CLUSTER_NAME":      "prod-1",
		"CURRENCY":          "USD",
		"PLUTUS_INGEST_URL": "https:///no-host",
	}))
	if err == nil {
		t.Fatal("expected an error for a malformed PLUTUS_INGEST_URL with no host")
	}
}

func TestLoad_RejectsMalformedOpenCostURL(t *testing.T) {
	_, err := Load(fakeEnv(map[string]string{
		"PLUTUS_API_KEY": "key123",
		"CLUSTER_NAME":   "prod-1",
		"CURRENCY":       "USD",
		"OPENCOST_URL":   "http:///no-host",
	}))
	if err == nil {
		t.Fatal("expected an error for a malformed OPENCOST_URL with no host")
	}
}

func TestLoad_HonorsOverrides(t *testing.T) {
	cfg, err := Load(fakeEnv(map[string]string{
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
