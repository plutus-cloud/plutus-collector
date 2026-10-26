// Package preflight checks, before a push and on demand, that the data pipeline behind OpenCost
// holds what the cost allocation needs: a reachable Prometheus-compatible store, OpenCost's own
// series in it, container usage from cAdvisor, one cluster's worth of data, enough history, and
// an OpenCost that computes allocations from all of that.
//
// Each check is one instant query (or one OpenCost call) with a stable id, a severity and an
// actionable message. The ids are an interface: they key the plutus_collector_preflight_ok
// metric, the `preflight` block sent to Plutus and the troubleshooting table in the README, so
// they are renamed only with all three.
package preflight

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/plutus-cloud/plutus-collector/internal/promclient"
)

// Check ids.
const (
	CheckPrometheusReachable = "prometheus_reachable"
	CheckOpenCostSeries      = "opencost_series"
	CheckAllocationSeries    = "allocation_series"
	CheckPodsRunning         = "pods_running"
	CheckPodLabels           = "pod_labels"
	CheckNamespaceLabels     = "namespace_labels"
	CheckControllerLabels    = "controller_labels"
	CheckUsageSeries         = "usage_series"
	CheckClusterAmbiguity    = "cluster_ambiguity"
	CheckHistory             = "history"
	CheckOpenCostReachable   = "opencost_reachable"
	CheckAllocationNonEmpty  = "allocation_nonempty"
	CheckPricing             = "pricing"
)

// Severities, as reported in the `preflight` block and the metric.
const (
	OK    = "ok"
	Warn  = "warn"
	Error = "error"
)

// Result is the outcome of one check.
type Result struct {
	ID       string
	Severity string
	Message  string
	Remedy   string
}

// Report is the outcome of a run. A check that does not apply (for example every Prometheus check
// when Prometheus itself is unreachable) is absent rather than reported as passed or failed.
type Report struct {
	Results []Result

	// OldestSample is the timestamp of the oldest OpenCost sample the store holds, within the
	// lookback the history check uses. Zero when unknown.
	OldestSample time.Time
}

// Summary maps check id to severity, the shape sent to Plutus and exported as metrics.
func (r *Report) Summary() map[string]string {
	m := make(map[string]string, len(r.Results))
	for _, res := range r.Results {
		m[res.ID] = res.Severity
	}
	return m
}

// Failures returns the results that are not OK, in check order.
func (r *Report) Failures() []Result {
	var out []Result
	for _, res := range r.Results {
		if res.Severity != OK {
			out = append(out, res)
		}
	}
	return out
}

// HasError reports whether any check is at error severity.
func (r *Report) HasError() bool {
	for _, res := range r.Results {
		if res.Severity == Error {
			return true
		}
	}
	return false
}

// Blocking returns the error-severity results whose ids the operator asked to stop pushes.
func (r *Report) Blocking(blockOn []string) []Result {
	var out []Result
	for _, res := range r.Results {
		if res.Severity != Error {
			continue
		}
		for _, id := range blockOn {
			if id == res.ID {
				out = append(out, res)
			}
		}
	}
	return out
}

// Querier is the part of promclient.Client the checks use.
type Querier interface {
	Query(ctx context.Context, expr string) ([]promclient.Sample, error)
}

// OpenCostProbe is the part of opencost.Client the checks use.
type OpenCostProbe interface {
	CountAllocations(ctx context.Context, window string) (int, error)
	Provider(ctx context.Context) (string, error)
}

// Config describes what to check.
type Config struct {
	// Prometheus is nil when there is no store to query (mode none, or no URL configured).
	Prometheus Querier
	// PrometheusExpected is true when a store should exist (any mode but none). With a nil
	// Prometheus it turns the silent skip into a warning, so a missing URL is visible.
	PrometheusExpected bool
	OpenCost           OpenCostProbe

	OpenCostURL string
	ClusterName string
	// ClusterLabel, when set, scopes every query to series whose label of that name equals
	// ClusterName. It is how a store shared by several clusters is told apart.
	ClusterLabel string
	// CatchUpDays sizes the history lookback.
	CatchUpDays int
	// PricingCustom says the operator configured OpenCost's rates themselves.
	PricingCustom bool
	// OldestDayStart is the start of the oldest day the agent will push, for the history check.
	OldestDayStart time.Time
	Now            func() time.Time
}

// Run executes every check that applies and returns the report. It never returns an error: a
// check that cannot complete is itself a result.
func Run(ctx context.Context, cfg Config) *Report {
	r := &runner{cfg: cfg, rep: &Report{}}
	if cfg.Now == nil {
		r.cfg.Now = time.Now
	}
	r.run(ctx)
	return r.rep
}

type runner struct {
	cfg Config
	rep *Report
}

func (r *runner) add(id, severity, message, remedy string) {
	r.rep.Results = append(r.rep.Results, Result{ID: id, Severity: severity, Message: message, Remedy: remedy})
}

func (r *runner) pass(id, message string) { r.add(id, OK, message, "") }

// sel builds a selector for metric, adding the cluster scope when one is configured.
func (r *runner) sel(metric string, matchers ...string) string {
	if r.cfg.ClusterLabel != "" {
		matchers = append(matchers, fmt.Sprintf("%s=%q", r.cfg.ClusterLabel, r.cfg.ClusterName))
	}
	if len(matchers) == 0 {
		return metric
	}
	return metric + "{" + strings.Join(matchers, ",") + "}"
}

func (r *runner) metricsTarget() string {
	if u, err := url.Parse(r.cfg.OpenCostURL); err == nil && u.Host != "" {
		return u.Host
	}
	return "OpenCost's :9003"
}

const scrapeRemedy = "Make the Prometheus OpenCost queries scrape OpenCost's metrics port (honor_labels: true, at most 60s interval). " +
	"prometheus.mode=bundled does this for you; for your own Prometheus see \"Use your own Prometheus\" in the README."

func (r *runner) run(ctx context.Context) {
	promOK := false
	if r.cfg.Prometheus != nil {
		promOK = r.checkPrometheus(ctx)
	} else if r.cfg.PrometheusExpected {
		r.add(CheckPrometheusReachable, Warn, "PROMETHEUS_URL is not set, so the metrics store behind OpenCost was not checked",
			"Set PROMETHEUS_URL (and credentials) to the same endpoint OpenCost queries to enable the series, history and cluster checks.")
	}
	if promOK {
		r.checkSeries(ctx)
	}
	r.checkOpenCost(ctx)
}

// count runs count(<expr>) and returns the number, 0 when the vector is empty.
func (r *runner) count(ctx context.Context, expr string) (float64, error) {
	s, err := r.cfg.Prometheus.Query(ctx, "count("+expr+")")
	if err != nil {
		return 0, err
	}
	if len(s) == 0 {
		return 0, nil
	}
	return s[0].Value, nil
}

func (r *runner) checkPrometheus(ctx context.Context) bool {
	_, err := r.cfg.Prometheus.Query(ctx, "up")
	if err == nil {
		r.pass(CheckPrometheusReachable, "the metrics store answered")
		return true
	}
	var he *promclient.HTTPError
	switch {
	case errors.As(err, &he) && (he.Status == http.StatusUnauthorized || he.Status == http.StatusForbidden):
		r.add(CheckPrometheusReachable, Error, fmt.Sprintf("the metrics store rejected the credentials (HTTP %d)", he.Status),
			"Check the username/password or token, and that the token has read (query) scope, for example metrics:read on Grafana Cloud.")
	case errors.As(err, &he) && he.Status == http.StatusNotFound:
		r.add(CheckPrometheusReachable, Error, "the metrics store answered 404 for /api/v1/query: the URL path is wrong",
			"Use the query API base: Grafana Cloud needs .../api/prom, Mimir usually .../prometheus, plain Prometheus the server root.")
	case errors.As(err, &he):
		r.add(CheckPrometheusReachable, Error, fmt.Sprintf("the metrics store answered HTTP %d: %s", he.Status, he.Body),
			"Check the store's own logs, and that the URL points at the query API.")
	default:
		r.add(CheckPrometheusReachable, Error, "the metrics store is unreachable: "+err.Error(),
			"Check the URL, DNS and network policy. With prometheus.mode=bundled, check that the Prometheus pod is Running (it can be Pending if it asked for a PVC and the cluster has no default StorageClass).")
	}
	return false
}

func (r *runner) checkSeries(ctx context.Context) {
	// check runs one count and turns "no series" into the given severity and message.
	check := func(id, metric string, matchers []string, sev, missing, remedy string) bool {
		n, err := r.count(ctx, r.sel(metric, matchers...))
		switch {
		case err != nil:
			r.add(id, Error, "could not query "+metric+": "+err.Error(), "Check the metrics store.")
			return false
		case n < 1:
			r.add(id, sev, missing, remedy)
			return false
		}
		r.pass(id, fmt.Sprintf("%s: %d series", metric, int(n)))
		return true
	}

	target := r.metricsTarget()
	scopeHint := ""
	if r.cfg.ClusterLabel != "" {
		scopeHint = fmt.Sprintf(" with %s=%q", r.cfg.ClusterLabel, r.cfg.ClusterName)
	}

	haveCost := check(CheckOpenCostSeries, "node_cpu_hourly_cost", nil, Error,
		fmt.Sprintf("no node_cpu_hourly_cost series%s: nothing is scraping OpenCost at %s", scopeHint, target), scrapeRemedy)
	check(CheckAllocationSeries, "container_cpu_allocation", nil, Error,
		fmt.Sprintf("no container_cpu_allocation series%s: OpenCost's allocation series are not reaching the store", scopeHint),
		scrapeRemedy+" If you filter OpenCost's metrics, container_cpu_allocation must be on the keep list.")
	check(CheckPodsRunning, "kube_pod_container_status_running", nil, Error,
		"no kube_pod_container_status_running series: without them OpenCost cannot tell which pods existed, so it computes no allocations",
		scrapeRemedy)
	check(CheckPodLabels, "kube_pod_labels", nil, Warn,
		"no kube_pod_labels series: pod labels will be missing, so cost tags and team attribution will be empty", scrapeRemedy)
	check(CheckNamespaceLabels, "kube_namespace_labels", nil, Warn,
		"no kube_namespace_labels series: namespace labels will be missing", scrapeRemedy)
	check(CheckControllerLabels, "deployment_match_labels", nil, Warn,
		"no deployment_match_labels series: costs cannot be attributed to a Deployment or StatefulSet, only to pods",
		scrapeRemedy+" Do not drop OpenCost's kube-state-metrics-style series because you run kube-state-metrics: it does not export these.")
	check(CheckUsageSeries, "container_cpu_usage_seconds_total", []string{`container!=""`, `pod!=""`}, Warn,
		"no container CPU usage series from cAdvisor: costs will be based on resource requests only, and a pod without requests will cost nothing",
		"Scrape the kubelet's /metrics/cadvisor endpoint (prometheus.mode=bundled does). On GKE Autopilot, EKS Fargate or under a strict NetworkPolicy the kubelet may not be reachable from pods.")

	r.checkClusterAmbiguity(ctx)
	if haveCost {
		r.checkHistory(ctx)
	}
}

// checkClusterAmbiguity refuses to attribute other clusters' costs to this one. With one
// cluster per store a missing cluster label is fine; with several and no label to tell them
// apart, OpenCost's default configuration merges them.
func (r *runner) checkClusterAmbiguity(ctx context.Context) {
	if r.cfg.ClusterLabel != "" {
		r.pass(CheckClusterAmbiguity, fmt.Sprintf("queries are scoped to %s=%q", r.cfg.ClusterLabel, r.cfg.ClusterName))
		return
	}
	// Group by the two label names clusters are conventionally told apart by, plus OpenCost's
	// own. A series without a label groups under "", so a single cluster yields one group.
	s, err := r.cfg.Prometheus.Query(ctx, "count(count by (cluster, cluster_id) (node_cpu_hourly_cost))")
	if err != nil {
		r.add(CheckClusterAmbiguity, Error, "could not check how many clusters the store holds: "+err.Error(), "Check the metrics store.")
		return
	}
	n := 0
	if len(s) > 0 {
		n = int(s[0].Value)
	}
	if n > 1 {
		r.add(CheckClusterAmbiguity, Error,
			fmt.Sprintf("this store holds node prices for %d clusters and no cluster label is configured: costs from the other clusters would be attributed to %q", n, r.cfg.ClusterName),
			"Set prometheus.external.clusterLabel to the label that identifies the cluster, with values equal to clusterName, so OpenCost filters to this cluster.")
		return
	}
	r.pass(CheckClusterAmbiguity, "the store holds one cluster")
}

func (r *runner) checkHistory(ctx context.Context) {
	days := r.cfg.CatchUpDays + 2
	expr := fmt.Sprintf("min_over_time(timestamp(group(%s))[%dd:10m])", r.sel("node_cpu_hourly_cost"), days)
	s, err := r.cfg.Prometheus.Query(ctx, expr)
	if err != nil || len(s) == 0 {
		// Not fatal and not a finding: older stores may reject the subquery. The absence of the
		// check is enough; the allocation check still catches a store with nothing in it.
		return
	}
	oldest := time.Unix(int64(s[0].Value), 0).UTC()
	r.rep.OldestSample = oldest

	if !r.cfg.OldestDayStart.IsZero() && oldest.After(r.cfg.OldestDayStart) {
		firstFull := time.Date(oldest.Year(), oldest.Month(), oldest.Day()+1, 0, 0, 0, 0, time.UTC)
		r.add(CheckHistory, Warn,
			fmt.Sprintf("the store holds %s of OpenCost history (since %s): the first complete day is %s",
				r.cfg.Now().Sub(oldest).Round(time.Minute), oldest.Format(time.RFC3339), firstFull.Format("2006-01-02")),
			"Nothing to fix: days before that have no data to push. If this cluster has been running OpenCost for longer, the store's retention or an emptyDir reschedule dropped it.")
		return
	}
	r.pass(CheckHistory, "the store holds the history the agent pushes")
}

func (r *runner) checkOpenCost(ctx context.Context) {
	if r.cfg.OpenCost == nil {
		return
	}
	provider, err := r.cfg.OpenCost.Provider(ctx)
	if err != nil {
		r.add(CheckOpenCostReachable, Error, "OpenCost is not answering: "+err.Error(),
			"Check that the OpenCost pod is Running and Ready (kubectl logs on it says what it is waiting for, most often its Prometheus) and that opencost.endpoint is right.")
		return
	}
	r.pass(CheckOpenCostReachable, "OpenCost answered")

	if strings.EqualFold(provider, "custom") && !r.cfg.PricingCustom {
		r.add(CheckPricing, Warn,
			"OpenCost detected no cloud provider and no custom rates are set, so it prices with its built-in default rates",
			"Set opencost.pricing.custom (CPU, RAM, storage prices per hour) to your real rates, or costs are an estimate shaped like a public-cloud price list.")
	} else {
		r.pass(CheckPricing, "provider: "+provider)
	}

	// Only meaningful when the series it depends on are there, or the error would just repeat
	// the checks above.
	if r.cfg.Prometheus != nil && !r.seriesPassed() {
		return
	}
	n, err := r.cfg.OpenCost.CountAllocations(ctx, "1h")
	switch {
	case err != nil:
		r.add(CheckAllocationNonEmpty, Error, "OpenCost could not compute allocations: "+err.Error(),
			"Check the OpenCost logs.")
	case n == 0:
		r.add(CheckAllocationNonEmpty, Error, "OpenCost is reachable and its series are present, but it computed no allocations for the last hour",
			"If OpenCost started within the last few minutes, wait a few scrape intervals and run again. Otherwise check the OpenCost logs; a store that returns the series but not the labels OpenCost expects produces this.")
	default:
		r.pass(CheckAllocationNonEmpty, fmt.Sprintf("OpenCost computed %d namespaces of allocations for the last hour", n))
	}
}

// seriesPassed reports whether the series checks that allocations depend on all passed.
func (r *runner) seriesPassed() bool {
	need := map[string]bool{CheckOpenCostSeries: false, CheckAllocationSeries: false, CheckPodsRunning: false}
	for _, res := range r.rep.Results {
		if _, ok := need[res.ID]; ok && res.Severity == OK {
			need[res.ID] = true
		}
	}
	for _, ok := range need {
		if !ok {
			return false
		}
	}
	return true
}
