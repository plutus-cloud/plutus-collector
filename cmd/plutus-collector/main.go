// Command plutus-collector is a long-lived process (deliberately not a Kubernetes CronJob — see
// the design doc's §2, "The agent") that periodically queries an in-cluster OpenCost instance's
// /allocation API and pushes the result to Plutus's kubernetes-cost ingest endpoint.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/plutus-cloud/plutus-collector/internal/config"
	"github.com/plutus-cloud/plutus-collector/internal/ingest"
	"github.com/plutus-cloud/plutus-collector/internal/metrics"
	"github.com/plutus-cloud/plutus-collector/internal/opencost"
	"github.com/plutus-cloud/plutus-collector/internal/pusher"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := config.Load(os.Getenv)
	if err != nil {
		logger.Error("startup failed", "error", err)
		os.Exit(1)
	}

	logger.Info("starting plutus-collector",
		"opencost_url", cfg.OpenCostURL,
		"plutus_ingest_url", cfg.PlutusIngestURL,
		"cluster_name", cfg.ClusterName,
		"currency", cfg.Currency,
		"push_interval", cfg.PushInterval.String(),
		"metrics_addr", cfg.MetricsAddr,
	)

	httpClient := &http.Client{Timeout: cfg.HTTPTimeout}
	ocClient := opencost.New(cfg.OpenCostURL, httpClient)
	ingestClient := ingest.NewClient(cfg.PlutusIngestURL, cfg.PlutusAPIKey, httpClient)
	metricsState := metrics.NewState()

	p := pusher.New(ocClient, ingestClient, metricsState, pusher.Config{
		ClusterName:    cfg.ClusterName,
		Currency:       cfg.Currency,
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
