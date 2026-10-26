package config

import "testing"

func TestLoadOpenCost_PreflightDefaults(t *testing.T) {
	cfg, err := LoadOpenCost(openCostEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Preflight.Enabled || len(cfg.Preflight.BlockOn) != 1 || cfg.Preflight.BlockOn[0] != "cluster_ambiguity" {
		t.Errorf("preflight defaults wrong: %+v", cfg.Preflight)
	}
	if cfg.Prometheus.URL != "" {
		t.Errorf("no Prometheus URL should be assumed, got %q", cfg.Prometheus.URL)
	}
}

func TestLoadOpenCost_PreflightOverrides(t *testing.T) {
	cfg, err := LoadOpenCost(openCostEnv(map[string]string{
		"PREFLIGHT_ENABLED": "false", "PREFLIGHT_BLOCK_ON": "none",
		"PROMETHEUS_URL": "https://prom.example/api/prom", "PROMETHEUS_USERNAME": "u", "PROMETHEUS_PASSWORD": "p",
		"PROMETHEUS_CLUSTER_LABEL": "cluster", "PROMETHEUS_TENANT_ID": "t",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Preflight.Enabled || len(cfg.Preflight.BlockOn) != 0 {
		t.Errorf("overrides not applied: %+v", cfg.Preflight)
	}
	if cfg.Prometheus.Username != "u" || cfg.Prometheus.ClusterLabel != "cluster" || cfg.Prometheus.TenantID != "t" {
		t.Errorf("prometheus config not read: %+v", cfg.Prometheus)
	}
}

func TestLoadOpenCost_PrometheusValidation(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"credentials in the url": {"PROMETHEUS_URL": "https://u:p@prom.example"},
		"not a url":              {"PROMETHEUS_URL": "prom.example"},
		"half basic auth":        {"PROMETHEUS_USERNAME": "u"},
		"two auth forms":         {"PROMETHEUS_USERNAME": "u", "PROMETHEUS_PASSWORD": "p", "PROMETHEUS_BEARER_TOKEN": "t"},
		"half mtls":              {"PROMETHEUS_CLIENT_CERT_FILE": "/c"},
		"label injection":        {"PROMETHEUS_CLUSTER_LABEL": `a"} or vector(1) #`},
		"bad enabled":            {"PREFLIGHT_ENABLED": "sometimes"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadOpenCost(openCostEnv(env)); err == nil {
				t.Error("expected a validation error")
			}
		})
	}
}

func TestLoadOpenCost_PrometheusCredentialsAreNeverEchoed(t *testing.T) {
	_, err := LoadOpenCost(openCostEnv(map[string]string{"PROMETHEUS_URL": "https://user:secretpw@prom.example"}))
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, leaked := range []string{"secretpw", "user:secret"} {
		if contains(err.Error(), leaked) {
			t.Errorf("error leaks %q: %v", leaked, err)
		}
	}
}

func TestLoadPreflight_NeedsNoAPIKeyOrCurrency(t *testing.T) {
	s, err := LoadPreflight(fakeEnv(map[string]string{"CLUSTER_NAME": "c", "PROMETHEUS_URL": "http://prom:9090"}))
	if err != nil {
		t.Fatal(err)
	}
	if s.ClusterName != "c" || s.Prometheus.URL != "http://prom:9090" || s.CatchUpDays != 3 {
		t.Errorf("unexpected settings %+v", s)
	}
	if _, err := LoadPreflight(fakeEnv(map[string]string{"PROMETHEUS_CLUSTER_LABEL": "cluster"})); err == nil {
		t.Error("a cluster label without CLUSTER_NAME has nothing to compare against and must be rejected")
	}
}
