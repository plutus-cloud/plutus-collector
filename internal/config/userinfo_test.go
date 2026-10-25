package config

import (
	"strings"
	"testing"
)

// Credentials in a URL end up in logs and proxies' access logs, so every URL the preflight
// subcommand and the agent read refuses them and never echoes the offending value.
func TestLoadPreflight_RejectsCredentialsInURLsAndNeverEchoesThem(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
	}{
		{"opencost url", map[string]string{"OPENCOST_URL": "http://admin:hunter2@opencost:9003"}},
		{"prometheus url", map[string]string{"PROMETHEUS_URL": "https://admin:hunter2@prom.example"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadPreflight(fakeEnv(tc.env))
			if err == nil {
				t.Fatal("expected the URL to be rejected")
			}
			if strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), "admin:") {
				t.Errorf("error echoes the credential: %v", err)
			}
		})
	}
}
