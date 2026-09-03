// Command payments-api serves the public payments API (§3).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/likhith2366/paylo/internal/config"
	"github.com/likhith2366/paylo/internal/db"
	"github.com/likhith2366/paylo/internal/httpx"
	"github.com/likhith2366/paylo/internal/payments"
	"github.com/likhith2366/paylo/internal/risk"
	"github.com/likhith2366/paylo/internal/vault"
	"github.com/redis/go-redis/v9"
)

func main() {
	if err := run(); err != nil {
		slog.Error("payments-api: fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx := context.Background()
	pool, err := db.Connect(ctx, cfg.DatabaseURL, cfg.DBMaxConns)
	if err != nil {
		return err
	}
	defer pool.Close()

	bank := payments.NewBankClient(cfg.BankSimulatorURL, cfg.BankTimeout)
	vaultClient := vault.NewPaymentsAdapter(
		vault.NewClient(cfg.VaultURL, cfg.VaultInternalSecret, cfg.VaultTimeout),
	)

	// Redis backs the velocity counters. A failure to connect is logged but not
	// fatal: velocity rules go quiet without it, and the rest of the risk
	// engine still runs. Refusing to start would turn a cache outage into a
	// payments outage, which is precisely the coupling §14.3 warns against.
	var counter *risk.Counter
	if opts, err := redis.ParseURL(cfg.RedisURL); err != nil {
		slog.Warn("payments-api: invalid REDIS_URL, velocity rules disabled", "error", err)
	} else {
		rdb := redis.NewClient(opts)
		pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		if err := rdb.Ping(pingCtx).Err(); err != nil {
			slog.Warn("payments-api: redis unreachable, velocity rules disabled", "error", err)
		} else {
			counter = risk.NewCounter(rdb)
			defer rdb.Close()
		}
		cancel()
	}

	assessor := risk.NewService(
		risk.NewEngine(nil),
		counter,
		risk.NewClient(cfg.FraudServiceURL, cfg.FraudTimeout),
	)

	handler := payments.NewHandler(payments.NewService(pool, bank, vaultClient, assessor))

	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(httpx.Trace)
	r.Use(httpx.LogRequests)

	// Unauthenticated: liveness and readiness must answer even when the
	// database is unhealthy, or Kubernetes cannot distinguish "starting up"
	// from "dependency down" and will restart pods that are working fine.
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		httpx.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	r.Get("/readyz", func(w http.ResponseWriter, req *http.Request) {
		pingCtx, cancel := context.WithTimeout(req.Context(), 2*time.Second)
		defer cancel()
		if err := pool.Ping(pingCtx); err != nil {
			httpx.JSON(w, http.StatusServiceUnavailable,
				map[string]string{"status": "degraded", "reason": "database unreachable"})
			return
		}
		httpx.JSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})

	r.Group(func(r chi.Router) {
		r.Use(httpx.AuthenticateAPIKey(pool))
		handler.Routes(r)
	})

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           r,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		// Must exceed the bank timeout, or this server would cut off a request
		// that is legitimately waiting on the processor.
		WriteTimeout: cfg.BankTimeout + 25*time.Second,
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		slog.Info("payments-api listening", "port", cfg.Port, "env", cfg.Env)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("payments-api: listen failed", "error", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	// Drain in-flight requests before exiting. Killing a pod mid-charge is
	// exactly how a charge ends up in an ambiguous state that reconciliation
	// then has to clean up — worth avoiding on a routine deploy.
	slog.Info("payments-api shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
