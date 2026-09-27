// Command api serves the admin and user HTTP APIs, health probes and the
// one-time administrator bootstrap subcommand.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/hchw/slogan/internal/api"
	"github.com/hchw/slogan/internal/bootstrap"
	"github.com/hchw/slogan/internal/config"
	"github.com/hchw/slogan/internal/logging"
	"golang.org/x/term"
)

func main() {
	bootstrapAdmin := flag.Bool("bootstrap-admin", false, "create the initial administrator and exit")
	migrateOnly := flag.Bool("migrate-only", false, "apply database migrations and exit (used by release jobs)")
	flag.Parse()

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

	if *migrateOnly {
		// Migrations already ran inside bootstrap.Build; exiting here keeps the
		// release migration step separate from serving traffic.
		log.Info("migrations applied; exiting")
		return
	}
	if *bootstrapAdmin {
		if err := runBootstrap(ctx, bundle, log); err != nil {
			log.Error("bootstrap failed", "error", err)
			os.Exit(1)
		}
		return
	}

	handler := api.New(bundle.DB, cfg, bundle.Auth, bundle.Provider, bundle.Model, bundle.Quota,
		bundle.Request, bundle.Usage, bundle.Policy, bundle.Eval, bundle.Audit).Handler()

	mux := http.NewServeMux()
	mux.Handle("/", handler)
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		// Metrics are internal-only and fail closed without a scrape token.
		bundle.Metrics.TokenHandler(cfg.MetricsToken)(w, r)
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := bundle.DB.Healthy(r.Context()); err != nil {
			http.Error(w, "db unavailable", http.StatusServiceUnavailable)
			return
		}
		if err := bundle.Redis.Healthy(r.Context()); err != nil {
			http.Error(w, "redis unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	srv := &http.Server{Addr: cfg.APIAddr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		log.Info("api listening", "addr", cfg.APIAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("api server error", "error", err)
			cancel()
		}
	}()
	<-ctx.Done()
	shutdownCtx, c := context.WithTimeout(context.Background(), 10*time.Second)
	defer c()
	_ = srv.Shutdown(shutdownCtx)
}

func runBootstrap(ctx context.Context, bundle *bootstrap.Bundle, log *slog.Logger) error {
	username := strings.TrimSpace(os.Getenv("ADMIN_USERNAME"))
	password := ""
	if path := strings.TrimSpace(os.Getenv("ADMIN_PASSWORD_FILE")); path != "" {
		info, err := os.Stat(path)
		if err != nil {
			return errors.New("cannot stat ADMIN_PASSWORD_FILE")
		}
		if info.Mode().Perm()&0o077 != 0 {
			return errors.New("ADMIN_PASSWORD_FILE permissions must be 0600 or stricter")
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return errors.New("cannot read ADMIN_PASSWORD_FILE")
		}
		password = strings.TrimSpace(string(b))
	} else {
		// Read from a terminal without echo, or from protected stdin in a
		// non-interactive deployment job. The password is never accepted via argv.
		fmt.Fprint(os.Stderr, "Enter initial administrator password: ")
		var b []byte
		var err error
		if term.IsTerminal(int(os.Stdin.Fd())) {
			b, err = term.ReadPassword(int(os.Stdin.Fd()))
			fmt.Fprintln(os.Stderr)
		} else {
			line, readErr := bufio.NewReader(os.Stdin).ReadString('\n')
			b, err = []byte(line), readErr
		}
		if err != nil && len(b) == 0 {
			return errors.New("failed to read initial administrator password")
		}
		password = strings.TrimSpace(string(b))
	}
	if username == "" || password == "" {
		return errors.New("ADMIN_USERNAME and password from ADMIN_PASSWORD_FILE or stdin are required")
	}
	admin, err := bundle.Auth.Bootstrap(ctx, username, password, "Initial Administrator")
	if err != nil {
		return err
	}
	log.Info("administrator created", "username", admin.Username, "id", admin.ID)
	return nil
}
