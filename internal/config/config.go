// Package config loads the pusher agent's configuration entirely from environment variables
// (12-factor — see plutus-collector's README.md and the design doc's §2 "The agent"). There is
// deliberately no config file: this component is small enough that env vars plus a Helm
// values.yaml -> env mapping is the whole story.
package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"time"
)

// Config is the fully-resolved, validated runtime configuration.
type Config struct {
	// OpenCostURL is the base URL of the in-cluster OpenCost instance. Defaults to the Service
	// name the bundled subchart creates (see chart/values.yaml's opencost.fullnameOverride),
	// so the common "helm install with the bundled OpenCost subchart" case needs no override.
	OpenCostURL string

	// PlutusIngestURL is where the batch is POSTed. Defaults to the live Hetzner deployment's
	// public ingest route (see docs/kubernetes-cost-allocation.md and the design doc's §1).
	PlutusIngestURL string

	// PlutusAPIKey authenticates the push (Authorization: Bearer <key>), minted per-account via
	// POST /api/accounts/:accountId/cost-sources/kubernetes/api-key (design doc §1). Required,
	// no default — there is no safe default for a credential.
	PlutusAPIKey string

	// ClusterName labels every row pushed by this agent instance when OpenCost's own row has no
	// cluster identity, and is always sent as the batch-level cluster_name. Required, no
	// default — per lib/sync/opencost.js's precedent, an unnamed cluster is a cosmetic problem
	// but silently mislabelling one cluster as another (by guessing a default) is not.
	ClusterName string

	// Currency is the ISO 4217 currency OpenCost was configured to price in. OpenCost does not
	// emit a currency field anywhere in its allocation output (confirmed against the CSV
	// exporter source, see docs/kubernetes-cost-allocation.md §9.6), so this can never be
	// defaulted to "USD" — that is the exact defect docs/multi-currency.md exists to prevent.
	// Required, no default.
	Currency string

	// PushInterval is how often the agent queries OpenCost and pushes a batch. Defaults to 24h,
	// matching OpenCost's own daily allocation-aggregation cadence (see design doc §2 and
	// docs/kubernetes-cost-allocation.md §7) — pushing more often than OpenCost re-aggregates
	// buys nothing.
	PushInterval time.Duration

	// MetricsAddr is the listen address for the /healthz and /metrics HTTP server.
	MetricsAddr string

	// HTTPTimeout bounds a single OpenCost query or Plutus push HTTP call.
	HTTPTimeout time.Duration

	// MaxRetries and RetryBaseDelay govern in-process retry/backoff on a failed push (see
	// internal/pusher). A network blip must not crash the whole process — see design doc §2's
	// CronJob-vs-Deployment discussion.
	MaxRetries     int
	RetryBaseDelay time.Duration
}

const (
	defaultOpenCostURL     = "http://opencost.opencost.svc.cluster.local:9003"
	defaultIngestURL       = "https://console.plutus-cloud.com/api/ingest/kubernetes-cost"
	defaultPushIntervalMin = 24 * 60 // daily, matching OpenCost's own aggregation cadence
	defaultMetricsAddr     = ":9100"
	defaultHTTPTimeout     = 60 * time.Second
	defaultMaxRetries      = 5
	defaultRetryBaseDelay  = 5 * time.Second
)

// Load reads and validates configuration from the process environment. It returns an error
// (rather than exiting) for every missing/invalid required value so main() can log a single
// clear startup failure instead of a panic.
func Load(getenv func(string) string) (*Config, error) {
	if getenv == nil {
		getenv = os.Getenv
	}

	cfg := &Config{
		OpenCostURL:     stringOrDefault(getenv("OPENCOST_URL"), defaultOpenCostURL),
		PlutusIngestURL: stringOrDefault(getenv("PLUTUS_INGEST_URL"), defaultIngestURL),
		PlutusAPIKey:    getenv("PLUTUS_API_KEY"),
		ClusterName:     getenv("CLUSTER_NAME"),
		Currency:        getenv("CURRENCY"),
		MetricsAddr:     stringOrDefault(getenv("METRICS_ADDR"), defaultMetricsAddr),
		HTTPTimeout:     defaultHTTPTimeout,
		MaxRetries:      defaultMaxRetries,
		RetryBaseDelay:  defaultRetryBaseDelay,
	}

	var errs []string

	if cfg.PlutusAPIKey == "" {
		errs = append(errs, "PLUTUS_API_KEY is required (no default — it is a credential)")
	}
	if cfg.ClusterName == "" {
		errs = append(errs, "CLUSTER_NAME is required (no default)")
	}
	if cfg.Currency == "" {
		errs = append(errs, "CURRENCY is required (no default — OpenCost does not report its own currency, see docs/multi-currency.md)")
	}

	if u, err := url.Parse(cfg.OpenCostURL); err != nil || u.Host == "" {
		errs = append(errs, fmt.Sprintf("OPENCOST_URL must be a well-formed URL with a host, got %q", cfg.OpenCostURL))
	}

	if u, err := url.Parse(cfg.PlutusIngestURL); err != nil || u.Host == "" {
		errs = append(errs, fmt.Sprintf("PLUTUS_INGEST_URL must be a well-formed URL with a host, got %q", cfg.PlutusIngestURL))
	} else if u.Scheme != "https" {
		// PlutusIngestURL carries a Bearer API key on every request; an http:// value would
		// send that key in cleartext, so unlike OpenCostURL (typically an in-cluster
		// http:// Service) this scheme is not optional.
		errs = append(errs, fmt.Sprintf("PLUTUS_INGEST_URL must use https (carries a Bearer API key), got %q", cfg.PlutusIngestURL))
	}

	intervalMin := defaultPushIntervalMin
	if raw := getenv("PUSH_INTERVAL_MINUTES"); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v <= 0 {
			errs = append(errs, fmt.Sprintf("PUSH_INTERVAL_MINUTES must be a positive integer, got %q", raw))
		} else {
			intervalMin = v
		}
	}
	cfg.PushInterval = time.Duration(intervalMin) * time.Minute

	if len(errs) > 0 {
		msg := "invalid configuration:"
		for _, e := range errs {
			msg += "\n  - " + e
		}
		return nil, fmt.Errorf("%s", msg)
	}

	return cfg, nil
}

func stringOrDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
