// Package config loads a collector's configuration entirely from environment variables
// (12-factor — see plutus-collector's README.md and the design doc's §2 "The agent"). There is
// deliberately no config file: this component is small enough that env vars plus a Helm
// values.yaml -> env mapping is the whole story.
//
// ─── ONE COMMON STRUCT, ONE LOADER PER BINARY ───────────────────────────────
//
// Common holds what every collector needs; LoadOpenCost and LoadLiteLLM each add their own
// required fields on top. The alternative — one Config with a MODE switch — was rejected because
// the required fields genuinely differ: CLUSTER_NAME and CURRENCY are mandatory for OpenCost and
// meaningless for LiteLLM. A mode switch would turn the collect-every-error validation below
// into a nest of conditionals, and the whole value of that design is that a misconfigured
// deployment gets ONE startup message listing everything wrong rather than failing one variable
// at a time.
package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Common is the configuration every collector in this repo needs, whatever it reads.
type Common struct {
	// PlutusIngestURL is where the batch is POSTed. Defaults to the live Hetzner deployment's
	// public ingest route (see docs/kubernetes-cost-allocation.md and the design doc's §1).
	PlutusIngestURL string

	// PlutusAPIKey authenticates the push (Authorization: Bearer <key>), minted per-account via
	// POST /api/accounts/:accountId/cost-sources/kubernetes/api-key (design doc §1). Required,
	// no default — there is no safe default for a credential.
	PlutusAPIKey string

	// PushInterval is how often the agent queries its source and pushes a batch. Defaults to
	// 24h, matching the daily aggregation cadence of both sources this repo reads (see design
	// doc §2 and docs/kubernetes-cost-allocation.md §7) — pushing more often than the source
	// re-aggregates buys nothing.
	PushInterval time.Duration

	// MetricsAddr is the listen address for the /healthz and /metrics HTTP server.
	MetricsAddr string

	// HTTPTimeout bounds a single source query or Plutus push HTTP call.
	HTTPTimeout time.Duration

	// MaxRetries and RetryBaseDelay govern in-process retry/backoff on a failed push (see
	// internal/pusher). A network blip must not crash the whole process — see design doc §2's
	// CronJob-vs-Deployment discussion.
	MaxRetries     int
	RetryBaseDelay time.Duration
}

// OpenCostConfig is Common plus what the Kubernetes collector needs.
type OpenCostConfig struct {
	Common

	// OpenCostURL is the base URL of the in-cluster OpenCost instance. Defaults to the Service
	// name the bundled subchart creates (see chart/values.yaml's opencost.fullnameOverride),
	// so the common "helm install with the bundled OpenCost subchart" case needs no override.
	OpenCostURL string

	// ClusterName labels every row pushed by this agent instance when OpenCost's own row has no
	// cluster identity, and is always sent as the batch-level cluster_name. Required, no
	// default — an unnamed cluster is a cosmetic problem, but silently mislabelling one cluster
	// as another (by guessing a default) is not.
	ClusterName string

	// Currency is the ISO 4217 currency OpenCost was configured to price in. OpenCost does not
	// emit a currency field anywhere in its allocation output (see
	// docs/kubernetes-cost-allocation.md §9.6), so this can never be defaulted to "USD" — that
	// is the exact defect docs/multi-currency.md exists to prevent. Required, no default.
	//
	// There is deliberately no counterpart in LiteLLMConfig: that source prices from a
	// USD-denominated map and the backend asserts the currency itself, so asking an operator for
	// one would invite a value nobody checks.
	Currency string
}

// LiteLLMConfig is Common plus what the LiteLLM collector needs.
type LiteLLMConfig struct {
	Common

	// LiteLLMBaseURL is the proxy's admin API, reachable on the customer's own network. No
	// default: unlike OpenCost there is no conventional in-cluster Service name, and this is
	// commonly not on Kubernetes at all.
	//
	// http:// is permitted here, unlike PlutusIngestURL — this is typically an internal address
	// and the request never leaves the customer's network. See the scheme check below.
	LiteLLMBaseURL string

	// LiteLLMMasterKey authenticates against the proxy's admin routes. Required, no default —
	// it is a credential, and an ordinary virtual key will 401 on /spend/logs.
	LiteLLMMasterKey string

	// ActivityTagPrefix enables the optional per-row `activity` label: a request tag starting
	// with this prefix supplies it. Empty means the feature is off and payloads are unchanged.
	// Set by --activity-tag-prefix (flag wins) or ACTIVITY_TAG_PREFIX; ACTIVITY_TAGS=true turns
	// it on with the conventional prefix "activity:".
	ActivityTagPrefix string
}

// DefaultActivityTagPrefix is the prefix used when the feature is enabled without naming one.
const DefaultActivityTagPrefix = "activity:"

// BifrostConfig is Common plus what the Bifrost collector needs.
type BifrostConfig struct {
	Common

	// BifrostBaseURL is the gateway's HTTP address (the one serving /api/logs), reachable on the
	// customer's own network. Required, no default. http:// is allowed, as for LiteLLM.
	BifrostBaseURL string

	// Management-API credentials. Bifrost's API is unauthenticated unless the operator enables
	// auth, and how it is authenticated was not confirmed against a live instance, so every form
	// is optional configuration: a header (APIKeyHeader: APIKeyScheme APIKey) and/or basic auth.
	APIKey       string
	APIKeyHeader string
	APIKeyScheme string
	BasicUser    string
	BasicPass    string

	// FieldOverrides maps a field name (see internal/bifrost.FieldMap) to a comma-separated list
	// of candidate dotted paths, from BIFROST_FIELD_<NAME>. Empty means the built-in defaults.
	FieldOverrides map[string][]string
}

// BifrostFieldNames are the overridable record fields, as the suffix of BIFROST_FIELD_<NAME>.
var BifrostFieldNames = []string{"TIMESTAMP", "STATUS", "PROVIDER", "MODEL", "VIRTUAL_KEY", "TEAM", "COST", "INPUT_TOKENS", "OUTPUT_TOKENS"}

const (
	defaultBifrostIngestURL = "https://console.plutus-cloud.com/api/ingest/bifrost-cost"
	defaultOpenCostURL      = "http://opencost.opencost.svc.cluster.local:9003"
	defaultIngestURL        = "https://console.plutus-cloud.com/api/ingest/kubernetes-cost"
	defaultLiteLLMIngestURL = "https://console.plutus-cloud.com/api/ingest/litellm-cost"
	defaultPushIntervalMin  = 24 * 60 // daily, matching OpenCost's own aggregation cadence
	defaultMetricsAddr      = ":9100"
	defaultHTTPTimeout      = 60 * time.Second
	defaultMaxRetries       = 5
	defaultRetryBaseDelay   = 5 * time.Second
)

// loadCommon fills the shared fields and appends any problems to errs, rather than returning
// early on the first one — see the package comment on why every collector reports its whole
// configuration failure at once.
func loadCommon(getenv func(string) string, defaultIngest string, errs *[]string) Common {
	c := Common{
		PlutusIngestURL: stringOrDefault(getenv("PLUTUS_INGEST_URL"), defaultIngest),
		PlutusAPIKey:    getenv("PLUTUS_API_KEY"),
		MetricsAddr:     stringOrDefault(getenv("METRICS_ADDR"), defaultMetricsAddr),
		HTTPTimeout:     defaultHTTPTimeout,
		MaxRetries:      defaultMaxRetries,
		RetryBaseDelay:  defaultRetryBaseDelay,
	}

	if c.PlutusAPIKey == "" {
		*errs = append(*errs, "PLUTUS_API_KEY is required (no default — it is a credential)")
	}

	if u, err := url.Parse(c.PlutusIngestURL); err != nil || u.Host == "" {
		*errs = append(*errs, fmt.Sprintf("PLUTUS_INGEST_URL must be a well-formed URL with a host, got %q", c.PlutusIngestURL))
	} else if u.Scheme != "https" {
		// PlutusIngestURL carries a Bearer API key on every request; an http:// value would
		// send that key in cleartext, so unlike a source URL (typically an internal http://
		// address) this scheme is not optional.
		*errs = append(*errs, fmt.Sprintf("PLUTUS_INGEST_URL must use https (carries a Bearer API key), got %q", c.PlutusIngestURL))
	}

	intervalMin := defaultPushIntervalMin
	if raw := getenv("PUSH_INTERVAL_MINUTES"); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v <= 0 {
			*errs = append(*errs, fmt.Sprintf("PUSH_INTERVAL_MINUTES must be a positive integer, got %q", raw))
		} else {
			intervalMin = v
		}
	}
	c.PushInterval = time.Duration(intervalMin) * time.Minute

	return c
}

func finish(errs []string) error {
	if len(errs) == 0 {
		return nil
	}
	msg := "invalid configuration:"
	for _, e := range errs {
		msg += "\n  - " + e
	}
	return fmt.Errorf("%s", msg)
}

// LoadOpenCost reads and validates the Kubernetes collector's configuration from the process
// environment. It returns an error (rather than exiting) for every missing/invalid required
// value so main() can log a single clear startup failure instead of a panic.
func LoadOpenCost(getenv func(string) string) (*OpenCostConfig, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	var errs []string

	cfg := &OpenCostConfig{
		Common:      loadCommon(getenv, defaultIngestURL, &errs),
		OpenCostURL: stringOrDefault(getenv("OPENCOST_URL"), defaultOpenCostURL),
		ClusterName: getenv("CLUSTER_NAME"),
		Currency:    getenv("CURRENCY"),
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

	if err := finish(errs); err != nil {
		return nil, err
	}
	return cfg, nil
}

// LoadLiteLLM reads and validates the LiteLLM collector's configuration, same contract as
// LoadOpenCost.
func LoadLiteLLM(getenv func(string) string) (*LiteLLMConfig, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	var errs []string

	cfg := &LiteLLMConfig{
		Common:           loadCommon(getenv, defaultLiteLLMIngestURL, &errs),
		LiteLLMBaseURL:   getenv("LITELLM_BASE_URL"),
		LiteLLMMasterKey: getenv("LITELLM_MASTER_KEY"),
	}
	if p := getenv("ACTIVITY_TAG_PREFIX"); p != "" {
		cfg.ActivityTagPrefix = p
	} else if v := strings.ToLower(getenv("ACTIVITY_TAGS")); v == "true" || v == "1" {
		cfg.ActivityTagPrefix = DefaultActivityTagPrefix
	}

	if cfg.LiteLLMMasterKey == "" {
		errs = append(errs, "LITELLM_MASTER_KEY is required (no default — it is a credential, and it must have admin scope)")
	}
	if cfg.LiteLLMBaseURL == "" {
		errs = append(errs, "LITELLM_BASE_URL is required (no default — there is no conventional address for a LiteLLM proxy)")
	} else if u, err := url.Parse(cfg.LiteLLMBaseURL); err != nil || u.Host == "" {
		errs = append(errs, fmt.Sprintf("LITELLM_BASE_URL must be a well-formed URL with a host, got %q", cfg.LiteLLMBaseURL))
	}

	if err := finish(errs); err != nil {
		return nil, err
	}
	return cfg, nil
}

// LoadBifrost reads and validates the Bifrost collector's configuration, same contract as
// LoadOpenCost.
func LoadBifrost(getenv func(string) string) (*BifrostConfig, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	var errs []string

	cfg := &BifrostConfig{
		Common:         loadCommon(getenv, defaultBifrostIngestURL, &errs),
		BifrostBaseURL: getenv("BIFROST_BASE_URL"),
		APIKey:         getenv("BIFROST_API_KEY"),
		APIKeyHeader:   stringOrDefault(getenv("BIFROST_API_KEY_HEADER"), "Authorization"),
		APIKeyScheme:   getenv("BIFROST_API_KEY_SCHEME"),
		BasicUser:      getenv("BIFROST_BASIC_USER"),
		BasicPass:      getenv("BIFROST_BASIC_PASSWORD"),
		FieldOverrides: map[string][]string{},
	}
	// "Bearer" is the default scheme only for the Authorization header; a custom header such as
	// X-Api-Key carries the raw value unless a scheme is set explicitly.
	if cfg.APIKeyScheme == "" && strings.EqualFold(cfg.APIKeyHeader, "Authorization") {
		cfg.APIKeyScheme = "Bearer"
	}

	if cfg.BifrostBaseURL == "" {
		errs = append(errs, "BIFROST_BASE_URL is required (no default — there is no conventional address for a Bifrost gateway)")
	} else if u, err := url.Parse(cfg.BifrostBaseURL); err != nil || u.Host == "" {
		errs = append(errs, fmt.Sprintf("BIFROST_BASE_URL must be a well-formed URL with a host, got %q", cfg.BifrostBaseURL))
	}
	if cfg.BasicUser != "" && cfg.APIKey != "" {
		errs = append(errs, "set either BIFROST_API_KEY or BIFROST_BASIC_USER/BIFROST_BASIC_PASSWORD, not both")
	}
	for _, name := range BifrostFieldNames {
		if raw := getenv("BIFROST_FIELD_" + name); raw != "" {
			var paths []string
			for _, p := range strings.Split(raw, ",") {
				if p = strings.TrimSpace(p); p != "" {
					paths = append(paths, p)
				}
			}
			cfg.FieldOverrides[name] = paths
		}
	}

	if err := finish(errs); err != nil {
		return nil, err
	}
	return cfg, nil
}

func stringOrDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
