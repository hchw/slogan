// Command worker runs background jobs: model evaluation refreshes and other
// asynchronous tasks.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hchw/slogan/internal/bootstrap"
	"github.com/hchw/slogan/internal/config"
	"github.com/hchw/slogan/internal/logging"
	"github.com/hchw/slogan/internal/worker"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config error:", err)
		os.Exit(2)
	}
	log := logging.New(cfg.Env)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	bundle, err := bootstrap.Build(ctx, cfg)
	if err != nil {
		log.Error("startup failed", "error", err)
		os.Exit(1)
	}
	defer bundle.Close()

	log.Info("worker started")
	if cfg.MetricsAddr != "" {
		// Background-job metrics are scraped from an internal listener only.
		metricsMux := http.NewServeMux()
		metricsMux.HandleFunc("/metrics", bundle.Metrics.TokenHandler(cfg.MetricsToken))
		metricsMux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
		srv := &http.Server{Addr: cfg.MetricsAddr, Handler: metricsMux, ReadHeaderTimeout: 10 * time.Second}
		go func() {
			log.Info("worker metrics listening", "addr", cfg.MetricsAddr)
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("worker metrics server error", "error", err)
			}
		}()
		defer func() {
			shutdownCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
			defer c()
			_ = srv.Shutdown(shutdownCtx)
		}()
	}
	go worker.RunSettlementRecovery(ctx, bundle.DB, bundle.Quota, bundle.Usage)
	bundle.Eval.RunWorker(ctx)
	log.Info("worker stopped")
}
