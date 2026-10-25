package preflight

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/plutus-cloud/plutus-collector/internal/promclient"
)

// fakeProm answers queries from a table keyed by a substring of the expression, so a test states
// which series exist rather than reproducing exact PromQL.
type fakeProm struct {
	// counts maps a metric name to its series count; absent means 0.
	counts  map[string]float64
	errFor  func(expr string) error
	queries []string
	// clusters is what the ambiguity query returns.
	clusters float64
	// oldest is the unix timestamp the history query returns; 0 means no result.
	oldest float64
}

func (f *fakeProm) Query(ctx context.Context, expr string) ([]promclient.Sample, error) {
	f.queries = append(f.queries, expr)
	if f.errFor != nil {
		if err := f.errFor(expr); err != nil {
			return nil, err
		}
	}
	switch {
	case expr == "up":
		return []promclient.Sample{{Value: 1}}, nil
	case strings.HasPrefix(expr, "count(count by"):
		return []promclient.Sample{{Value: f.clusters}}, nil
	case strings.HasPrefix(expr, "min_over_time"):
		if f.oldest == 0 {
			return nil, nil
		}
		return []promclient.Sample{{Value: f.oldest}}, nil
	}
	for metric, n := range f.counts {
		if strings.Contains(expr, metric+"{") || strings.Contains(expr, "("+metric+")") {
			if n == 0 {
				return nil, nil
			}
			return []promclient.Sample{{Value: n}}, nil
		}
	}
	return nil, nil
}

type fakeOpenCost struct {
	allocs   int
	provider string
	err      error
}

func (f *fakeOpenCost) CountAllocations(ctx context.Context, window string) (int, error) {
	return f.allocs, f.err
}
func (f *fakeOpenCost) Provider(ctx context.Context) (string, error) { return f.provider, f.err }

func healthyProm() *fakeProm {
	return &fakeProm{
		counts: map[string]float64{
			"node_cpu_hourly_cost": 3, "container_cpu_allocation": 40, "kube_pod_container_status_running": 40,
			"kube_pod_labels": 40, "kube_namespace_labels": 6, "deployment_match_labels": 9,
			"container_cpu_usage_seconds_total": 40,
		},
		clusters: 1,
	}
}

func severities(r *Report) map[string]string { return r.Summary() }

func baseConfig(p Querier, oc OpenCostProbe) Config {
	return Config{
		Prometheus: p, OpenCost: oc, OpenCostURL: "http://oc.ns.svc:9003", ClusterName: "prod",
		CatchUpDays: 3, Now: func() time.Time { return time.Date(2026, 10, 25, 2, 0, 0, 0, time.UTC) },
		OldestDayStart: time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC),
	}
}

func TestRun_HealthyEverythingPasses(t *testing.T) {
	p := healthyProm()
	p.oldest = float64(time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC).Unix())
	rep := Run(context.Background(), baseConfig(p, &fakeOpenCost{allocs: 5, provider: "GCP"}))

	for id, sev := range severities(rep) {
		if sev != OK {
			t.Errorf("%s = %s, want ok (%+v)", id, sev, rep.Failures())
		}
	}
	for _, id := range []string{CheckPrometheusReachable, CheckOpenCostSeries, CheckPodsRunning, CheckClusterAmbiguity, CheckHistory, CheckAllocationNonEmpty, CheckPricing} {
		if _, ok := severities(rep)[id]; !ok {
			t.Errorf("check %s did not run", id)
		}
	}
	if rep.HasError() {
		t.Error("a healthy report must have no errors")
	}
}

func TestRun_NothingScrapesOpenCost(t *testing.T) {
	p := healthyProm()
	p.counts["node_cpu_hourly_cost"] = 0
	p.counts["container_cpu_allocation"] = 0
	rep := Run(context.Background(), baseConfig(p, &fakeOpenCost{allocs: 0, provider: "GCP"}))

	s := severities(rep)
	if s[CheckOpenCostSeries] != Error || s[CheckAllocationSeries] != Error {
		t.Fatalf("expected opencost_series and allocation_series errors: %v", s)
	}
	var msg string
	for _, r := range rep.Results {
		if r.ID == CheckOpenCostSeries {
			msg = r.Message + " " + r.Remedy
		}
	}
	if !strings.Contains(msg, "oc.ns.svc:9003") || !strings.Contains(msg, "honor_labels") {
		t.Errorf("message should name the scrape target and the fix, got %q", msg)
	}
	if _, ran := s[CheckAllocationNonEmpty]; ran {
		t.Error("allocation_nonempty must not repeat a failure the series checks already explain")
	}
}

func TestRun_PrometheusFailuresAreClassified(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"401", &promclient.HTTPError{Status: 401}, "rejected the credentials"},
		{"403", &promclient.HTTPError{Status: 403}, "rejected the credentials"},
		{"404", &promclient.HTTPError{Status: 404}, "path is wrong"},
		{"500", &promclient.HTTPError{Status: 500, Body: "boom"}, "HTTP 500"},
		{"dns", errors.New("dial tcp: lookup prometheus-server: no such host"), "unreachable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &fakeProm{errFor: func(string) error { return tc.err }}
			rep := Run(context.Background(), baseConfig(p, nil))
			if len(rep.Results) != 1 || rep.Results[0].ID != CheckPrometheusReachable || rep.Results[0].Severity != Error {
				t.Fatalf("unexpected results %+v", rep.Results)
			}
			if !strings.Contains(rep.Results[0].Message, tc.want) {
				t.Errorf("message %q does not mention %q", rep.Results[0].Message, tc.want)
			}
		})
	}
}

func TestRun_SeveralClustersWithoutALabelBlocks(t *testing.T) {
	p := healthyProm()
	p.clusters = 2
	rep := Run(context.Background(), baseConfig(p, &fakeOpenCost{allocs: 5, provider: "GCP"}))

	if severities(rep)[CheckClusterAmbiguity] != Error {
		t.Fatalf("expected cluster_ambiguity error: %v", severities(rep))
	}
	if got := rep.Blocking([]string{CheckClusterAmbiguity}); len(got) != 1 {
		t.Errorf("expected one blocking result, got %v", got)
	}
	if got := rep.Blocking([]string{CheckUsageSeries}); len(got) != 0 {
		t.Errorf("only listed ids block, got %v", got)
	}
}

func TestRun_AClusterLabelScopesEveryQueryAndSatisfiesTheAmbiguityCheck(t *testing.T) {
	p := healthyProm()
	p.clusters = 3
	cfg := baseConfig(p, &fakeOpenCost{allocs: 5, provider: "GCP"})
	cfg.ClusterLabel = "cluster"
	rep := Run(context.Background(), cfg)

	if severities(rep)[CheckClusterAmbiguity] != OK {
		t.Errorf("a configured label resolves the ambiguity: %v", rep.Failures())
	}
	for _, q := range p.queries {
		if strings.HasPrefix(q, "count(") && !strings.HasPrefix(q, "count(count by") && !strings.Contains(q, `cluster="prod"`) {
			t.Errorf("query is not scoped to the cluster: %s", q)
		}
	}
}

func TestRun_MissingUsageAndLabelsWarnWithoutFailing(t *testing.T) {
	p := healthyProm()
	p.counts["container_cpu_usage_seconds_total"] = 0
	p.counts["kube_pod_labels"] = 0
	p.counts["deployment_match_labels"] = 0
	rep := Run(context.Background(), baseConfig(p, &fakeOpenCost{allocs: 5, provider: "GCP"}))

	s := severities(rep)
	for _, id := range []string{CheckUsageSeries, CheckPodLabels, CheckControllerLabels} {
		if s[id] != Warn {
			t.Errorf("%s = %s, want warn", id, s[id])
		}
	}
	if rep.HasError() {
		t.Errorf("warnings must not count as errors: %+v", rep.Failures())
	}
}

func TestRun_ShortHistoryWarnsAndNamesTheFirstCompleteDay(t *testing.T) {
	p := healthyProm()
	p.oldest = float64(time.Date(2026, 10, 24, 15, 0, 0, 0, time.UTC).Unix())
	rep := Run(context.Background(), baseConfig(p, &fakeOpenCost{allocs: 5, provider: "GCP"}))

	if severities(rep)[CheckHistory] != Warn {
		t.Fatalf("expected a history warning: %v", severities(rep))
	}
	var msg string
	for _, r := range rep.Results {
		if r.ID == CheckHistory {
			msg = r.Message
		}
	}
	if !strings.Contains(msg, "2026-10-25") {
		t.Errorf("message should name the first complete day, got %q", msg)
	}
	if !rep.OldestSample.Equal(time.Date(2026, 10, 24, 15, 0, 0, 0, time.UTC)) {
		t.Errorf("oldest sample not recorded: %v", rep.OldestSample)
	}
}

func TestRun_OpenCostComputingNothingIsAnError(t *testing.T) {
	rep := Run(context.Background(), baseConfig(healthyProm(), &fakeOpenCost{allocs: 0, provider: "GCP"}))
	if severities(rep)[CheckAllocationNonEmpty] != Error {
		t.Errorf("expected allocation_nonempty error: %v", severities(rep))
	}
}

func TestRun_OpenCostUnreachable(t *testing.T) {
	rep := Run(context.Background(), baseConfig(healthyProm(), &fakeOpenCost{err: errors.New("connection refused")}))
	if severities(rep)[CheckOpenCostReachable] != Error {
		t.Errorf("expected opencost_reachable error: %v", severities(rep))
	}
}

func TestRun_DefaultRatesWarnUnlessCustomPricingIsSet(t *testing.T) {
	oc := &fakeOpenCost{allocs: 5, provider: "custom"}
	if got := severities(Run(context.Background(), baseConfig(healthyProm(), oc)))[CheckPricing]; got != Warn {
		t.Errorf("custom provider with no rates set = %s, want warn", got)
	}
	cfg := baseConfig(healthyProm(), oc)
	cfg.PricingCustom = true
	if got := severities(Run(context.Background(), cfg))[CheckPricing]; got != OK {
		t.Errorf("custom provider with rates set = %s, want ok", got)
	}
}

func TestRun_WithoutAStoreOnlyOpenCostIsChecked(t *testing.T) {
	cfg := baseConfig(nil, &fakeOpenCost{allocs: 2, provider: "custom"})
	cfg.PricingCustom = true
	rep := Run(context.Background(), cfg)
	s := severities(rep)
	if _, ok := s[CheckPrometheusReachable]; ok {
		t.Error("no Prometheus checks should run without a store")
	}
	if s[CheckAllocationNonEmpty] != OK || s[CheckOpenCostReachable] != OK {
		t.Errorf("OpenCost checks should still run: %v", s)
	}
}
