// Command plutus-collector is a long-lived process (deliberately not a Kubernetes CronJob) that periodically queries an in-cluster OpenCost instance's
// /allocation API and pushes the result to Plutus's kubernetes-cost ingest endpoint.
//
// Run with --once it does a single pass and exits, which is what manual backfills and the
// end-to-end tests use.
package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/plutus-cloud/plutus-collector/internal/config"
	"github.com/plutus-cloud/plutus-collector/internal/ingest"
	"github.com/plutus-cloud/plutus-collector/internal/k8scost"
	"github.com/plutus-cloud/plutus-collector/internal/metrics"
	"github.com/plutus-cloud/plutus-collector/internal/opencost"
	"github.com/plutus-cloud/plutus-collector/internal/pusher"
	"github.com/plutus-cloud/plutus-collector/internal/version"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	// "plutus-collector preflight" runs the data-pipeline checks once and exits. It is a
	// subcommand rather than a flag because it is a different job from pushing, with its own
	// (smaller) configuration.
	if len(os.Args) > 1 && os.Args[1] == "preflight" {
		os.Exit(runPreflight(logger))
	}

	once := flag.Bool("once", false, "run one pass and exit (non-zero if any window failed or came back empty), instead of running as a long-lived agent")
	windowFlag := flag.String("window", "", "with --once: push this window instead of the recent closed UTC days, as <start>,<end> RFC3339 timestamps. Rows are dated by the start's UTC day and a push replaces that whole day in Plutus, so use whole UTC days for backfills")
	flag.Parse()

	if *windowFlag != "" && !*once {
		logger.Error("startup failed", "error", "--window requires --once")
		os.Exit(2)
	}

	cfg, err := config.LoadOpenCost(os.Getenv)
	if err != nil {
		logger.Error("startup failed", "error", err)
		os.Exit(1)
	}

	logger.Info("starting plutus-collector",
		"version", version.Version,
		"opencost_url", cfg.OpenCostURL,
		"plutus_ingest_url", cfg.PlutusIngestURL,
		"cluster_name", cfg.ClusterName,
		"currency", cfg.Currency,
		"prometheus_mode", cfg.PrometheusMode,
		"catch_up_days", cfg.CatchUpDays,
		"metrics_addr", cfg.MetricsAddr,
	)

	httpClient := &http.Client{Timeout: cfg.HTTPTimeout}
	source := &k8scost.Source{
		Allocations: opencost.New(cfg.OpenCostURL, httpClient),
		ClusterName: cfg.ClusterName,
		Currency:    cfg.Currency,
		EmptyOK:     cfg.EmptyResultIgnore,
	}
	ingestHTTP, err := ingest.NewHTTPClient(cfg.HTTPTimeout, cfg.PlutusIngestCAFile)
	if err != nil {
		logger.Error("startup failed", "error", err)
		os.Exit(1)
	}
	ingestClient := ingest.NewClient(cfg.PlutusIngestURL, cfg.PlutusAPIKey, ingestHTTP)
	metricsState := metrics.NewState()

	pcfg := pusher.Config{
		PushInterval:   cfg.PushInterval,
		CatchUpDays:    cfg.CatchUpDays,
		MaxRetries:     cfg.MaxRetries,
		RetryBaseDelay: cfg.RetryBaseDelay,
		Version:        version.Version,
		PrometheusMode: cfg.PrometheusMode,
	}
	if cfg.PushIntervalSet {
		logger.Warn("PUSH_INTERVAL_MINUTES is set, so the daily UTC schedule is not used; it is deprecated, prefer PUSH_SCHEDULE_UTC",
			"push_interval", cfg.PushInterval.String())
	} else {
		pcfg.Schedule = &pusher.Schedule{
			Hour:   cfg.ScheduleHour,
			Minute: cfg.ScheduleMinute,
			Jitter: time.Duration(cfg.JitterMinutes) * time.Minute,
		}
	}

	p := pusher.New(source, ingestClient, metricsState, pcfg, logger)
	if cfg.Preflight.Enabled {
		checker, err := newChecker(cfg.Prometheus, cfg.PrometheusMode, cfg.OpenCostURL, cfg.ClusterName, cfg.CatchUpDays, cfg.Preflight.PricingCustom, httpClient)
		if err != nil {
			logger.Error("startup failed", "error", err)
			os.Exit(1)
		}
		p.Preflight = checker
		p.Config.BlockOn = cfg.Preflight.BlockOn
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *once {
		os.Exit(runOnce(ctx, p, *windowFlag, logger))
	}

	metricsServer := &http.Server{
		Addr:              cfg.MetricsAddr,
		Handler:           metricsState.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		logger.Info("metrics/health server listening", "addr", cfg.MetricsAddr)
		if err := metricsServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("metrics server failed", "error", err)
		}
	}()

	go p.Run(ctx)

	<-ctx.Done()
	logger.Info("shutdown signal received, stopping")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := metricsServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("error shutting down metrics server", "error", err)
	}
}

// runOnce processes one set of windows and returns the process exit code: 0 when every window
// was delivered, 1 when any failed or came back empty where that is a fault. It starts no HTTP
// server, so it can run inside the pod of a running agent (kubectl exec) without contending for
// the metrics port.
func runOnce(ctx context.Context, p *pusher.Pusher, windowFlag string, logger *slog.Logger) int {
	windows := pusher.ClosedDayWindows(time.Now(), p.Config.CatchUpDays)
	if windowFlag != "" {
		w, err := pusher.ParseWindow(windowFlag)
		if err != nil {
			logger.Error("invalid --window", "error", err)
			return 2
		}
		windows = []pusher.Window{w}
	}

	exit := 0
	for _, o := range p.RunOnce(ctx, windows) {
		if o.Failed() {
			exit = 1
		}
	}
	return exit
}
