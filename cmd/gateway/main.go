// Command gateway serves the OpenAI-compatible data plane.
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
	"github.com/hchw/slogan/internal/gateway"
	"github.com/hchw/slogan/internal/httpx"
	"github.com/hchw/slogan/internal/laya"
	"github.com/hchw/slogan/internal/logging"
	"github.com/hchw/slogan/internal/routing"
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

	nodeID := os.Getenv("GATEWAY_NODE_ID")
	if nodeID == "" {
		nodeID = fmt.Sprintf("gw-%d", time.Now().UnixNano())
	}
	lc := laya.FromConfig(cfg.Laya)
	router := routing.NewRouter(bundle.Model, bundle.Policy, bundle.Eval, bundle.DB)
	svc := gateway.New(bundle.DB, cfg, bundle.Auth, bundle.Model, bundle.Provider, bundle.Quota,
		bundle.Request, bundle.Usage, router, lc, bundle.Redis, bundle.Policy)

	svc.SetMetrics(bundle.Metrics.Counters())
	lc.SetMetrics(bundle.Metrics.Counters())
	go svc.RunNode(ctx, nodeID)

	mux := http.NewServeMux()
	mux.Handle("/", svc.Handler())
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		bundle.Metrics.TokenHandler(cfg.MetricsToken)(w, r)
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := bundle.DB.Healthy(r.Context()); err != nil {
			http.Error(w, "db unavailable", http.StatusServiceUnavailable)
			return
		}
		if err := bundle.Redis.Healthy(r.Context()); err != nil {
			http.Error(w, "redis unavailable", http.StatusServiceUnavailable)
			return
		}
		nodes, err := bundle.Redis.Nodes(r.Context())
		state, exists := nodes[nodeID]
		if err != nil || !exists || !state.Ready || state.Drain {
			http.Error(w, "gateway node is not ready", http.StatusServiceUnavailable)
			return
		}
		// The node still serves traffic when the classifier is unavailable; the
		// readiness body must say so instead of claiming full capability.
		classifier := laya.ReadyState{}
		if cfg.Laya.Enabled {
			classifier = lc.Ready(r.Context())
		}
		httpx.WriteData(w, r, http.StatusOK, map[string]any{
			"nodeId": nodeID, "scoreVersion": state.Version, "ready": true,
			"classifierEnabled": cfg.Laya.Enabled,
			"classifierReady":   !cfg.Laya.Enabled || classifier.ClassifierReady,
			"degraded":          cfg.Laya.Enabled && !classifier.ClassifierReady,
			"classifierVersion": classifier.Version,
			"classifierReason":  classifier.Reason,
		})
	})

	srv := &http.Server{Addr: cfg.GatewayAddr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		log.Info("gateway listening", "addr", cfg.GatewayAddr, "node", nodeID)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("gateway server error", "error", err)
			cancel()
		}
	}()
	<-ctx.Done()
	shutdownCtx, c := context.WithTimeout(context.Background(), 10*time.Second)
	defer c()
	_ = srv.Shutdown(shutdownCtx)
}
