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

func TestLoadLiteLLM_ActivityPrefixIsOffByDefaultAndOptInViaEnv(t *testing.T) {
	base := map[string]string{
		"PLUTUS_API_KEY": "key123", "LITELLM_BASE_URL": "http://litellm:4000", "LITELLM_MASTER_KEY": "sk-master",
	}
	cfg, err := LoadLiteLLM(fakeEnv(base))
	if err != nil || cfg.ActivityTagPrefix != "" {
		t.Fatalf("expected off by default, got %q err=%v", cfg.ActivityTagPrefix, err)
	}
	base["ACTIVITY_TAGS"] = "true"
	if cfg, _ = LoadLiteLLM(fakeEnv(base)); cfg.ActivityTagPrefix != "activity:" {
		t.Errorf("ACTIVITY_TAGS=true should use the default prefix, got %q", cfg.ActivityTagPrefix)
	}
	base["ACTIVITY_TAG_PREFIX"] = "use-case="
	if cfg, _ = LoadLiteLLM(fakeEnv(base)); cfg.ActivityTagPrefix != "use-case=" {
		t.Errorf("explicit prefix should win, got %q", cfg.ActivityTagPrefix)
	}
}

// ─── Bifrost ────────────────────────────────────────────────────────────────

func TestLoadBifrost_RequiresKeyAndBaseURL(t *testing.T) {
	_, err := LoadBifrost(fakeEnv(map[string]string{}))
	if err == nil || !contains(err.Error(), "PLUTUS_API_KEY") || !contains(err.Error(), "BIFROST_BASE_URL") {
		t.Fatalf("expected both to be named, got %v", err)
	}
}

func TestLoadBifrost_DefaultsAndAuthOptional(t *testing.T) {
	cfg, err := LoadBifrost(fakeEnv(map[string]string{"PLUTUS_API_KEY": "k", "BIFROST_BASE_URL": "http://bifrost:8080"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PlutusIngestURL != defaultBifrostIngestURL || cfg.APIKey != "" {
		t.Errorf("unexpected defaults: %+v", cfg)
	}
	cfg, _ = LoadBifrost(fakeEnv(map[string]string{"PLUTUS_API_KEY": "k", "BIFROST_BASE_URL": "http://b", "BIFROST_API_KEY": "t", "BIFROST_API_KEY_HEADER": "X-Api-Key"}))
	if cfg.APIKeyScheme != "" {
		t.Errorf("a custom header carries the raw value, got scheme %q", cfg.APIKeyScheme)
	}
}

func TestLoadBifrost_RejectsBothAuthFormsAndPlainHttpIngest(t *testing.T) {
	base := map[string]string{"PLUTUS_API_KEY": "k", "BIFROST_BASE_URL": "http://b", "BIFROST_API_KEY": "t", "BIFROST_BASIC_USER": "u"}
	if _, err := LoadBifrost(fakeEnv(base)); err == nil {
		t.Error("expected both auth forms to be rejected")
	}
	delete(base, "BIFROST_BASIC_USER")
	base["PLUTUS_INGEST_URL"] = "http://console.plutus-cloud.com/api/ingest/bifrost-cost"
	if _, err := LoadBifrost(fakeEnv(base)); err == nil {
		t.Error("expected a plain-http ingest URL to be rejected")
	}
}

// ─── credentials in URLs ────────────────────────────────────────────────────

func TestURLsWithUserinfoAreRejectedAndNeverEchoed(t *testing.T) {
	const secret = "hunter2-" + "SECRET"
	base := map[string]string{"PLUTUS_API_KEY": "k"}
	cases := map[string]func(map[string]string) (error, string){
		"BIFROST_BASE_URL": func(e map[string]string) (error, string) {
			e["BIFROST_BASE_URL"] = "http://admin:" + secret + "@bifrost:8080"
			_, err := LoadBifrost(fakeEnv(e))
			return err, "BIFROST_BASIC_USER"
		},
		"LITELLM_BASE_URL": func(e map[string]string) (error, string) {
			e["LITELLM_BASE_URL"] = "http://admin:" + secret + "@litellm:4000"
			e["LITELLM_MASTER_KEY"] = "sk"
			_, err := LoadLiteLLM(fakeEnv(e))
			return err, "LITELLM_MASTER_KEY"
		},
		"PLUTUS_INGEST_URL": func(e map[string]string) (error, string) {
			e["BIFROST_BASE_URL"] = "http://b"
			e["PLUTUS_INGEST_URL"] = "https://u:" + secret + "@console.plutus-cloud.com/x"
			_, err := LoadBifrost(fakeEnv(e))
			return err, "PLUTUS_API_KEY"
		},
		"OPENCOST_URL": func(e map[string]string) (error, string) {
			e["CLUSTER_NAME"], e["CURRENCY"] = "c", "USD"
			e["OPENCOST_URL"] = "http://u:" + secret + "@oc:9003"
			_, err := LoadOpenCost(fakeEnv(e))
			return err, "credentials"
		},
	}
	for name, run := range cases {
		env := map[string]string{}
		for k, v := range base {
			env[k] = v
		}
		err, hint := run(env)
		if err == nil || !contains(err.Error(), name) || !contains(err.Error(), "credentials") || !contains(err.Error(), hint) {
			t.Errorf("%s: expected a rejection naming the variable and %q, got %v", name, hint, err)
			continue
		}
		if contains(err.Error(), secret) {
			t.Errorf("%s: error leaked the password: %v", name, err)
		}
	}
}

func TestMalformedURLErrorsDoNotEchoTheValue(t *testing.T) {
	const pw = "pw-" + "SECRET"
	_, err := LoadBifrost(fakeEnv(map[string]string{"PLUTUS_API_KEY": "k", "BIFROST_BASE_URL": "http://u:" + pw + "@ho st"}))
	if err == nil || contains(err.Error(), pw) {
		t.Errorf("want an error without the value, got %v", err)
	}
}
