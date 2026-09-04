// Command scheduler runs the periodic jobs (§9).
//
// Currently reconciliation. Payouts, idempotency-key cleanup, and dunning
// retries belong here as they are built.
//
// Jobs are run in-process on a ticker rather than via cron, so a job that
// overruns its interval delays the next tick instead of overlapping with
// itself. Reconciliation is idempotent and would survive an overlap, but
// relying on that is worse than not creating the situation.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/likhith2366/paylo/internal/config"
	"github.com/likhith2366/paylo/internal/db"
	"github.com/likhith2366/paylo/internal/payments"
	"github.com/likhith2366/paylo/internal/payouts"
	"github.com/likhith2366/paylo/internal/reconcile"
)

func main() {
	if err := run(); err != nil {
		slog.Error("scheduler: fatal", "error", err)
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

	bank := payments.NewBankClient(cfg.BankSimulatorURL, cfg.BankTimeout)
	reconciler := reconcile.NewService(pool, bank)

	payoutSvc := payouts.NewService(pool, bank)

	reconcileEvery := envDuration("RECONCILE_INTERVAL", time.Hour)
	payoutEvery := envDuration("PAYOUT_INTERVAL", 24*time.Hour)

	reconcileTick := time.NewTicker(reconcileEvery)
	defer reconcileTick.Stop()
	payoutTick := time.NewTicker(payoutEvery)
	defer payoutTick.Stop()

	slog.Info("scheduler started",
		"reconcile_interval", reconcileEvery.String(),
		"payout_interval", payoutEvery.String())

	// Run once at startup rather than waiting a full interval — a restart
	// after an incident is exactly when there is most likely to be something
	// parked and waiting.
	runReconciliation(ctx, reconciler)

	for {
		select {
		case <-ctx.Done():
			slog.Info("scheduler shutting down")
			return nil
		case <-reconcileTick.C:
			runReconciliation(ctx, reconciler)
		case <-payoutTick.C:
			runPayouts(ctx, payoutSvc)
		}
	}
}

func runPayouts(ctx context.Context, p *payouts.Service) {
	jobCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()

	result, err := p.Run(jobCtx)
	if err != nil {
		slog.Error("scheduler: payout run failed", "error", err)
		return
	}
	slog.Info("scheduler: payouts complete",
		"merchants_considered", result.MerchantsConsidered,
		"payouts_created", result.PayoutsCreated,
		"total_cents", result.TotalCents,
		"skipped", result.Skipped)
}

func runReconciliation(ctx context.Context, r *reconcile.Service) {
	// Bounded so a hung processor cannot stall the scheduler indefinitely.
	jobCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()

	result, err := r.Run(jobCtx)
	if err != nil {
		slog.Error("scheduler: reconciliation failed", "error", err)
		return
	}

	slog.Info("scheduler: reconciliation complete",
		"run_id", result.RunID,
		"charges_checked", result.ChargesChecked,
		"charges_resolved", result.ChargesResolved,
		"discrepancies", result.Discrepancies)
}

func envDuration(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}
