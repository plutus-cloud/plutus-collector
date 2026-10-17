// Command plutus-bifrost-collector is a long-lived process that periodically reads a self-hosted
// Bifrost gateway's (github.com/maximhq/bifrost) request-log API and pushes a daily aggregate to
// Plutus's bifrost-cost ingest endpoint.
//
// A sibling of cmd/plutus-litellm-collector, sharing its transport, retry policy, metrics and
// loop (internal/ingest, internal/pusher, internal/metrics). Same reason for existing at all: the
// gateway sits inside the customer's network, where nothing of Plutus's can reach it, so the
// collector runs beside it and pushes out. Nothing per-request crosses the wire.
//
// UNVERIFIED against a real Bifrost: see internal/bifrost's package comment and the README.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/plutus-cloud/plutus-collector/internal/bifrost"
	"github.com/plutus-cloud/plutus-collector/internal/config"
	"github.com/plutus-cloud/plutus-collector/internal/ingest"
	"github.com/plutus-cloud/plutus-collector/internal/metrics"
	"github.com/plutus-cloud/plutus-collector/internal/pusher"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := config.LoadBifrost(os.Getenv)
	if err != nil {
		logger.Error("startup failed", "error", err)
		os.Exit(1)
	}

	// No credential of any kind in this line, and no field-override values beyond their names.
	logger.Info("starting plutus-bifrost-collector",
		"bifrost_base_url", cfg.BifrostBaseURL,
		"plutus_ingest_url", cfg.PlutusIngestURL,
		"push_interval", cfg.PushInterval.String(),
		"metrics_addr", cfg.MetricsAddr,
		"bifrost_auth_configured", cfg.APIKey != "" || cfg.BasicUser != "",
		"field_overrides", len(cfg.FieldOverrides),
	)

	httpClient := &http.Client{Timeout: cfg.HTTPTimeout}
	source := &bifrost.Source{
		Logs:   bifrost.New(cfg.BifrostBaseURL, bifrost.AuthFromConfig(cfg), bifrost.FieldsFromConfig(cfg), httpClient),
		Logger: logger,
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
