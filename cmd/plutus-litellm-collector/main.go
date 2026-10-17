// Command plutus-litellm-collector is a long-lived process that periodically reads a
// self-hosted LiteLLM proxy's admin spend API and pushes a daily aggregate to Plutus's
// litellm-cost ingest endpoint.
//
// A sibling of cmd/plutus-collector rather than a mode of it: the two share this repo's
// transport, retry policy, metrics and loop (internal/ingest, internal/pusher,
// internal/metrics), and differ in the two things that genuinely differ — which system they read
// and which environment variables are mandatory. See internal/config's package comment.
//
// Why a push agent at all, when every other Plutus AI cost source is polled: a self-hosted
// gateway sits inside the customer's network, where nothing of Plutus's can reach it. Same
// constraint as OpenCost, same answer — the collector runs on their side and pushes out. Nothing
// per-request crosses the wire; the aggregation happens here.
package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/plutus-cloud/plutus-collector/internal/config"
	"github.com/plutus-cloud/plutus-collector/internal/ingest"
	"github.com/plutus-cloud/plutus-collector/internal/litellm"
	"github.com/plutus-cloud/plutus-collector/internal/metrics"
	"github.com/plutus-cloud/plutus-collector/internal/pusher"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	activityFlag := flag.String("activity-tag-prefix", "",
		"enable the per-row `activity` label from request tags starting with this prefix (e.g. \"activity:\"); off when unset. Overrides ACTIVITY_TAG_PREFIX")
	flag.Parse()

	cfg, err := config.LoadLiteLLM(os.Getenv)
	if err != nil {
		logger.Error("startup failed", "error", err)
		os.Exit(1)
	}
	if *activityFlag != "" {
		cfg.ActivityTagPrefix = *activityFlag
	}

	// The master key is deliberately absent from this line. Everything else about the
	// configuration is worth having in a support transcript; a credential is not.
	logger.Info("starting plutus-litellm-collector",
		"litellm_base_url", redact(cfg.LiteLLMBaseURL),
		"plutus_ingest_url", redact(cfg.PlutusIngestURL),
		"push_interval", cfg.PushInterval.String(),
		"metrics_addr", cfg.MetricsAddr,
		"activity_tag_prefix", cfg.ActivityTagPrefix,
	)

	httpClient := &http.Client{Timeout: cfg.HTTPTimeout}
	source := &litellm.Source{
		Logs:              litellm.New(cfg.LiteLLMBaseURL, cfg.LiteLLMMasterKey, httpClient),
		ActivityTagPrefix: cfg.ActivityTagPrefix,
	}
	ingestClient := ingest.NewClient(cfg.PlutusIngestURL, cfg.PlutusAPIKey, httpClient)
	metricsState := metrics.NewState()

	p := pusher.New(source, ingestClient, metricsState, pusher.Config{
		PushInterval:   cfg.PushInterval,
		MaxRetries:     cfg.MaxRetries,
		RetryBaseDelay: cfg.RetryBaseDelay,
	}, logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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

// redact strips any password from a URL before it is logged. Config validation already refuses
// userinfo; this is the second line of defence.
func redact(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		return u.Redacted()
	}
	return "(unparseable url)"
}
