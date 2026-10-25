package config

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
)

// PrometheusConfig is where the metrics store behind OpenCost is and how to authenticate to it.
// The agent never needs it to compute costs (OpenCost does that); it is used by the preflight
// checks, which ask the same store the same questions OpenCost asks. Point it at the endpoint
// OpenCost uses, with the same credentials.
type PrometheusConfig struct {
	// URL is the query API base. Empty when there is no store to check (PROMETHEUS_MODE=none, or
	// an operator who has not said where it is), in which case the Prometheus checks are skipped.
	URL string

	Username, Password string
	BearerToken        string
	TenantID           string

	InsecureSkipVerify bool
	CAFile             string
	CertFile, KeyFile  string

	// ClusterLabel names the label that identifies the cluster in a store shared by several.
	// Its values must equal CLUSTER_NAME. Empty means one cluster per store.
	ClusterLabel string
}

// PreflightConfig controls the checks themselves.
type PreflightConfig struct {
	// Enabled runs the checks at startup and before each push (PREFLIGHT_ENABLED, default true).
	Enabled bool

	// BlockOn lists check ids whose failure stops the push, rather than only being reported
	// (PREFLIGHT_BLOCK_ON, comma separated, default cluster_ambiguity; "none" blocks nothing).
	BlockOn []string

	// PricingCustom says the operator set OpenCost's rates themselves, so the pricing check does
	// not warn that OpenCost is using its built-in defaults (PRICING_CUSTOM_CONFIGURED).
	PricingCustom bool
}

var labelName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

func loadPrometheus(getenv func(string) string, errs *[]string) PrometheusConfig {
	p := PrometheusConfig{
		URL:                getenv("PROMETHEUS_URL"),
		Username:           getenv("PROMETHEUS_USERNAME"),
		Password:           getenv("PROMETHEUS_PASSWORD"),
		BearerToken:        getenv("PROMETHEUS_BEARER_TOKEN"),
		TenantID:           getenv("PROMETHEUS_TENANT_ID"),
		InsecureSkipVerify: isTrue(getenv("PROMETHEUS_INSECURE_SKIP_VERIFY")),
		CAFile:             getenv("PROMETHEUS_CA_FILE"),
		CertFile:           getenv("PROMETHEUS_CLIENT_CERT_FILE"),
		KeyFile:            getenv("PROMETHEUS_CLIENT_KEY_FILE"),
		ClusterLabel:       getenv("PROMETHEUS_CLUSTER_LABEL"),
	}

	if p.URL != "" {
		if u, err := url.Parse(p.URL); err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			*errs = append(*errs, "PROMETHEUS_URL must be an http(s) URL with a host (value not echoed: it may contain credentials)")
		} else {
			rejectUserinfo("PROMETHEUS_URL", u, "use PROMETHEUS_USERNAME / PROMETHEUS_PASSWORD or PROMETHEUS_BEARER_TOKEN", errs)
		}
	}
	if (p.Username == "") != (p.Password == "") {
		*errs = append(*errs, "set both PROMETHEUS_USERNAME and PROMETHEUS_PASSWORD, or neither")
	}
	if p.BearerToken != "" && p.Username != "" {
		*errs = append(*errs, "set either PROMETHEUS_BEARER_TOKEN or PROMETHEUS_USERNAME/PROMETHEUS_PASSWORD, not both")
	}
	if (p.CertFile == "") != (p.KeyFile == "") {
		*errs = append(*errs, "set both PROMETHEUS_CLIENT_CERT_FILE and PROMETHEUS_CLIENT_KEY_FILE, or neither")
	}
	// The label name is interpolated into PromQL, so it is held to Prometheus's own label-name
	// grammar rather than escaped.
	if p.ClusterLabel != "" && !labelName.MatchString(p.ClusterLabel) {
		*errs = append(*errs, fmt.Sprintf("PROMETHEUS_CLUSTER_LABEL must be a valid Prometheus label name, got %q", p.ClusterLabel))
	}
	return p
}

func loadPreflight(getenv func(string) string, errs *[]string) PreflightConfig {
	c := PreflightConfig{
		Enabled:       true,
		BlockOn:       []string{"cluster_ambiguity"},
		PricingCustom: isTrue(getenv("PRICING_CUSTOM_CONFIGURED")),
	}
	if raw := strings.ToLower(getenv("PREFLIGHT_ENABLED")); raw != "" {
		switch raw {
		case "true", "1":
		case "false", "0":
			c.Enabled = false
		default:
			*errs = append(*errs, fmt.Sprintf("PREFLIGHT_ENABLED must be true or false, got %q", raw))
		}
	}
	if raw := getenv("PREFLIGHT_BLOCK_ON"); raw != "" {
		c.BlockOn = nil // "none" (or any list with no ids) means no check stops a push
		for _, id := range strings.Split(raw, ",") {
			if id = strings.TrimSpace(id); id != "" && id != "none" {
				c.BlockOn = append(c.BlockOn, id)
			}
		}
	}
	return c
}

// PreflightSettings is what the `preflight` subcommand needs. It deliberately does not require
// the API key or currency: running the checks should work before the rest is configured.
type PreflightSettings struct {
	OpenCostURL    string
	ClusterName    string
	CatchUpDays    int
	PrometheusMode string
	Prometheus     PrometheusConfig
	Preflight      PreflightConfig
}

// LoadPreflight reads the configuration for the `preflight` subcommand.
func LoadPreflight(getenv func(string) string) (*PreflightSettings, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	var errs []string
	s := &PreflightSettings{
		OpenCostURL:    stringOrDefault(getenv("OPENCOST_URL"), defaultOpenCostURL),
		ClusterName:    getenv("CLUSTER_NAME"),
		CatchUpDays:    defaultCatchUpDays,
		PrometheusMode: stringOrDefault(getenv("PROMETHEUS_MODE"), defaultPrometheusMode),
		Prometheus:     loadPrometheus(getenv, &errs),
		Preflight:      loadPreflight(getenv, &errs),
	}
	if u, err := url.Parse(s.OpenCostURL); err != nil || u.Host == "" {
		errs = append(errs, "OPENCOST_URL must be a well-formed URL with a host (value not echoed: it may contain credentials)")
	}
	if s.Prometheus.ClusterLabel != "" && s.ClusterName == "" {
		errs = append(errs, "CLUSTER_NAME is required when PROMETHEUS_CLUSTER_LABEL is set (its value is what the label must equal)")
	}
	if raw := getenv("CATCH_UP_DAYS"); raw != "" {
		var v int
		if _, err := fmt.Sscanf(raw, "%d", &v); err != nil || v < 1 || v > maxCatchUpDays {
			errs = append(errs, fmt.Sprintf("CATCH_UP_DAYS must be an integer from 1 to %d, got %q", maxCatchUpDays, raw))
		} else {
			s.CatchUpDays = v
		}
	}
	if err := finish(errs); err != nil {
		return nil, err
	}
	return s, nil
}

func isTrue(v string) bool {
	v = strings.ToLower(v)
	return v == "true" || v == "1"
}
