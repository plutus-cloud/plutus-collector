package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/plutus-cloud/plutus-collector/internal/config"
	"github.com/plutus-cloud/plutus-collector/internal/opencost"
	"github.com/plutus-cloud/plutus-collector/internal/preflight"
	"github.com/plutus-cloud/plutus-collector/internal/promclient"
	"github.com/plutus-cloud/plutus-collector/internal/pusher"
)

// newChecker wires the preflight checks from configuration. A store is only queried when one is
// configured and the mode says there is one; mode "none" has no Prometheus by design.
func newChecker(prom config.PrometheusConfig, mode, openCostURL, clusterName string, catchUpDays int, pricingCustom bool, httpClient *http.Client) (*preflight.Checker, error) {
	base := preflight.Config{
		OpenCost:           opencost.New(openCostURL, httpClient),
		OpenCostURL:        openCostURL,
		ClusterName:        clusterName,
		ClusterLabel:       prom.ClusterLabel,
		CatchUpDays:        catchUpDays,
		PricingCustom:      pricingCustom,
		PrometheusExpected: mode != "none",
	}
	if prom.URL != "" && mode != "none" {
		client, err := promclient.New(promclient.Config{
			URL:                prom.URL,
			Username:           prom.Username,
			Password:           prom.Password,
			BearerToken:        prom.BearerToken,
			TenantID:           prom.TenantID,
			InsecureSkipVerify: prom.InsecureSkipVerify,
			CAFile:             prom.CAFile,
			CertFile:           prom.CertFile,
			KeyFile:            prom.KeyFile,
		})
		if err != nil {
			return nil, err
		}
		base.Prometheus = client
	}
	return &preflight.Checker{Base: base}, nil
}

// runPreflight is the `preflight` subcommand: run every check once, print the result for a
// person, and exit non-zero if any check is at error severity. Warnings do not fail it. It is
// what `helm test` runs, and what `kubectl exec ... preflight` runs by hand.
func runPreflight(logger *slog.Logger) int {
	cfg, err := config.LoadPreflight(os.Getenv)
	if err != nil {
		logger.Error("invalid configuration", "error", err)
		return 2
	}
	checker, err := newChecker(cfg.Prometheus, cfg.PrometheusMode, cfg.OpenCostURL, cfg.ClusterName, cfg.CatchUpDays, cfg.Preflight.PricingCustom, &http.Client{Timeout: 30 * time.Second})
	if err != nil {
		logger.Error("could not set up the checks", "error", err)
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	rep := checker.Run(ctx, pusher.ClosedDayWindows(time.Now(), cfg.CatchUpDays)[0].Start)
	preflight.Print(os.Stdout, rep)

	if rep.HasError() {
		fmt.Fprintln(os.Stdout, "\npreflight failed: fix the errors above; nothing will be attributed correctly until they pass")
		return 1
	}
	fmt.Fprintln(os.Stdout, "\npreflight passed")
	return 0
}
