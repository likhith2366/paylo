// Command vault is the card tokenization service (§2.4).
//
// This is the only process in the system that ever holds a raw card number. It
// is deliberately small: less code here means less to review, less to get
// wrong, and less that a breach could reach. Resist adding anything to this
// service that does not strictly need to see a PAN.
//
// In production it runs in its own network segment, with its own IAM role and
// its own KMS key, reachable only by the checkout iframe (tokenize) and the
// charge-submission path (detokenize).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/likhith2366/paylo/internal/db"
	"github.com/likhith2366/paylo/internal/httpx"
	"github.com/likhith2366/paylo/internal/vault"
)

func main() {
	if err := run(); err != nil {
		slog.Error("vault: fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	env := getenv("PAYLO_ENV", "development")
	port := envInt("PORT", 8081)
	dsn := getenv("DATABASE_URL", "postgres://paylo:paylo@localhost:5432/paylo?sslmode=disable")

	masterKeyB64 := os.Getenv("VAULT_MASTER_KEY")
	internalSecret := os.Getenv("VAULT_INTERNAL_SECRET")

	// Refuse to start in production with development defaults. A vault running
	// on a known key is worse than no vault, because it looks protected.
	if env == "production" {
		if masterKeyB64 == "" {
			return errors.New("vault: VAULT_MASTER_KEY is required in production")
		}
		if internalSecret == "" {
			return errors.New("vault: VAULT_INTERNAL_SECRET is required in production")
		}
	}
	if masterKeyB64 == "" {
		generated, err := vault.NewMasterKey()
		if err != nil {
			return err
		}
		masterKeyB64 = generated
		// Ephemeral by design: restarting the vault in development makes
		// previously issued tokens undecryptable, which is correct — they were
		// never meant to outlive a dev session.
		slog.Warn("vault: no VAULT_MASTER_KEY set, generated an ephemeral development key")
	}
	if internalSecret == "" {
		internalSecret = "dev-only-internal-secret"
		slog.Warn("vault: using the default development internal secret")
	}

	masterKey, err := vault.ParseMasterKey(masterKeyB64)
	if err != nil {
		return err
	}
	keys, err := vault.NewLocalKeyManager(masterKey)
	if err != nil {
		return err
	}

	ctx := context.Background()
	pool, err := db.Connect(ctx, dsn, int32(envInt("DB_MAX_CONNS", 10)))
	if err != nil {
		return err
	}
	defer pool.Close()

	hashSalt := getenv("CARD_HASH_SALT", "dev-only-insecure-salt")
	ttl := envDuration("VAULT_TOKEN_TTL", 15*time.Minute)
	svc := vault.NewService(pool, keys, hashSalt, ttl)
	handler := vault.NewHandler(svc, internalSecret)

	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(httpx.Trace)
	// Note: no request-body logging middleware here, ever. Tokenize request
	// bodies contain card numbers.
	r.Use(httpx.LogRequests)

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		httpx.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// Origins permitted to embed the card frame and post to it. In production
	// this comes from registered merchant domains, not an env var — a merchant
	// must not be able to add their own origin without going through
	// onboarding.
	allowedOrigins := splitAndTrim(getenv("CHECKOUT_ALLOWED_ORIGINS",
		"http://localhost:3000 http://localhost:5173 http://localhost:8000"))

	r.Group(func(r chi.Router) {
		r.Use(vault.CORS(allowedOrigins))
		handler.Routes(r)
	})

	// The hosted card-input frame, served from the vault's own origin — which
	// is what makes the browser's same-origin policy the PCI boundary (§2.4).
	checkout, err := vault.CheckoutHandler(allowedOrigins)
	if err != nil {
		return fmt.Errorf("vault: mount checkout assets: %w", err)
	}
	r.Mount("/checkout", http.StripPrefix("/checkout", checkout))

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", port),
		Handler:           r,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// Expired tokens are purged on a schedule so the vault holds no more card
	// data than it currently needs (§9).
	purgeCtx, stopPurge := context.WithCancel(ctx)
	defer stopPurge()
	go runPurgeLoop(purgeCtx, svc)

	go func() {
		slog.Info("vault listening", "port", port, "env", env, "token_ttl", ttl.String())
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("vault: listen failed", "error", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	slog.Info("vault shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

func runPurgeLoop(ctx context.Context, svc *vault.Service) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			purgeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			n, err := svc.PurgeExpired(purgeCtx)
			cancel()
			if err != nil {
				slog.Error("vault: purge failed", "error", err)
				continue
			}
			if n > 0 {
				slog.Info("vault: purged expired tokens", "count", n)
			}
		}
	}
}

// splitAndTrim parses a whitespace- or comma-separated origin list.
func splitAndTrim(s string) []string {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n'
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
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
