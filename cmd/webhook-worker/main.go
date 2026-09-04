// Command webhook-worker drains the outbox and delivers events to merchants
// (§7, §22.1).
//
// Runs both halves of the pipeline in one process: the outbox poller that turns
// committed business events into queued deliveries, and the delivery worker
// that posts them. They are separate goroutines because they have different
// failure characteristics — the poller only touches the database, while the
// worker's latency is hostage to merchant servers — but a single deployable
// keeps the local stack simple. Splitting them is a one-line change if the
// delivery side needs to scale independently.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/likhith2366/paylo/internal/config"
	"github.com/likhith2366/paylo/internal/db"
	"github.com/likhith2366/paylo/internal/webhook"
)

func main() {
	if err := run(); err != nil {
		slog.Error("webhook-worker: fatal", "error", err)
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.Connect(ctx, cfg.DatabaseURL, cfg.DBMaxConns)
	if err != nil {
		return err
	}
	defer pool.Close()

	poller := webhook.NewPoller(pool,
		envInt("OUTBOX_BATCH", 100),
		envDuration("OUTBOX_INTERVAL", time.Second),
	)
	worker := webhook.NewWorker(pool,
		envInt("WEBHOOK_BATCH", 50),
		envInt("WEBHOOK_CONCURRENCY", 10),
		envDuration("WEBHOOK_POLL", 2*time.Second),
	)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); poller.Run(ctx) }()
	go func() { defer wg.Done(); worker.Run(ctx) }()

	slog.Info("webhook-worker started",
		"outbox_batch", envInt("OUTBOX_BATCH", 100),
		"delivery_concurrency", envInt("WEBHOOK_CONCURRENCY", 10),
		"max_attempts", webhook.MaxAttempts)

	<-ctx.Done()
	slog.Info("webhook-worker shutting down")

	// Let in-flight deliveries finish rather than abandoning them. An
	// abandoned delivery is not lost — its lease lapses and another worker
	// retries it — but finishing cleanly avoids a spurious duplicate to the
	// merchant on every deploy.
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		slog.Warn("webhook-worker: shutdown timed out with deliveries in flight")
	}
	return nil
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}
